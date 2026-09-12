package devorchestrator

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// The independent Review Worker (T0012 requirement 5): a genuine separate
// claude -p process with no Git control-plane permissions and NO write scope
// — it reviews the collected diff from inputs written into its own result
// dir, produces a verdict against specs/orchestrator/review-verdict.schema.json,
// and its verdict is recorded as gate evidence for the merge gate (review
// required for merge unless the task overrides it). The review does not trust
// the Worker's word: collect verifies the reviewed code did not change during
// the review (spawn-time fingerprint of the task worktree) and that the
// verdict schema-validates.

// ReviewTaskID returns the review pseudo-task id used for the review
// Worker's runtime dirs (registry, result dir, authoritative inputs).
func ReviewTaskID(taskID string) string { return taskID + "-review" }

// ReviewSpawnOpts parametrizes one Review Worker dispatch.
type ReviewSpawnOpts struct {
	RepoRoot     string
	DagPath      string
	StatePath    string
	TaskID       string // the task under review
	Model        string
	MaxTurns     *int
	MaxBudgetUSD *float64
	Timeout      time.Duration
	RunID        string
	ClaudeBin    string
}

// ReviewVerdict is the parsed review verdict document
// (specs/orchestrator/review-verdict.schema.json).
type ReviewVerdict struct {
	TaskID   string `json:"task_id"`
	Verdict  string `json:"verdict"`
	Summary  string `json:"summary"`
	Findings []struct {
		Severity string `json:"severity"`
		File     string `json:"file,omitempty"`
		Line     *int   `json:"line,omitempty"`
		Finding  string `json:"finding"`
	} `json:"findings"`
	Risks []string `json:"risks"`
}

// ReviewCollectReport is the outcome of collecting a Review Worker.
type ReviewCollectReport struct {
	TaskID  string         `json:"task_id"`
	Status  string         `json:"status"` // ok | rejected | failed
	Checks  []CollectCheck `json:"checks"`
	Reasons []string       `json:"reasons,omitempty"`
	Verdict *ReviewVerdict `json:"verdict,omitempty"`
}

// SpawnReview dispatches the independent Review Worker. The reviewed diff,
// the task package and the collect report are copied into the review result
// dir; the Reviewer reads only those inputs. The task's state must be
// verification — a review never runs against uncollected work.
func SpawnReview(opts *ReviewSpawnOpts) (*SpawnResult, error) {
	if opts == nil || opts.RepoRoot == "" || opts.TaskID == "" {
		return nil, fmt.Errorf("review spawn requires a repo root and task id")
	}
	store, err := OpenStore(opts.DagPath, opts.StatePath)
	if err != nil {
		return nil, err
	}
	insp, err := store.Inspect(opts.TaskID)
	if err != nil {
		return nil, err
	}
	// verification is the normal state to review from. accepted is allowed
	// too, and deliberately: what a review needs is a collected diff that has
	// not moved since, which is equally true of an accepted task. Without
	// this, a task accepted before its review requirement was noticed could
	// never be reviewed and therefore never merged — accepted and unmergeable
	// at once. `rddev task accept` now refuses in that order, so this is a
	// recovery affordance rather than the normal path, and it weakens nothing:
	// the verdict is still demanded by the merge gate, and a
	// request_changes verdict still blocks the merge.
	if insp.State.Status != StateVerification && insp.State.Status != StateAccepted {
		return nil, fmt.Errorf("task %s is %s, not verification or accepted — a Review Worker reviews a collected diff only (rddev worker collect first)", opts.TaskID, insp.State.Status)
	}
	rec, err := LoadRegistry(opts.RepoRoot, opts.TaskID)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("no recorded Worker for %s — nothing to review", opts.TaskID)
	}
	gate, err := LoadGateInputs(opts.RepoRoot, opts.TaskID)
	if err != nil {
		return nil, err
	}
	if gate == nil {
		return nil, fmt.Errorf("no authoritative spawn record for %s — refuse to review without the spawn-time baseline", opts.TaskID)
	}

	claudeBin := opts.ClaudeBin
	if claudeBin == "" {
		claudeBin = "claude"
	}
	if _, err := exec.LookPath(claudeBin); err != nil {
		return nil, fmt.Errorf("claude CLI not found on PATH (looked for %q): %w", claudeBin, err)
	}

	reviewID := ReviewTaskID(opts.TaskID)
	reviewDir := WorkerTaskDir(opts.RepoRoot, reviewID)
	// The review scratch "worktree" is a plain directory the Reviewer's shell
	// writes are confined to — it is NOT the task worktree, so the reviewed
	// code is out of the Reviewer's write envelope by construction.
	scratch := filepath.Join(WorktreesDir(opts.RepoRoot), reviewID)
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(reviewDir, 0o755); err != nil {
		return nil, err
	}

	// Fingerprint the reviewed worktree BEFORE the Reviewer exists: collect
	// rejects a verdict produced against different code.
	fp, err := taskWorktreeFingerprint(rec, gate.BaselineSHA)
	if err != nil {
		return nil, fmt.Errorf("fingerprinting the reviewed worktree: %w", err)
	}

	// Review inputs: the diff, the task package, the collect report. All
	// inside the review result dir (the only readable copy the Reviewer needs).
	diffText, err := taskWorktreeDiff(rec)
	if err != nil {
		return nil, err
	}
	inputs := map[string]string{
		"diff.txt": diffText,
		"collect-report.json": func() string {
			if data, err := os.ReadFile(filepath.Join(rec.ResultDir, "collect-report.json")); err == nil {
				return string(data)
			}
			return "{}\n"
		}(),
	}
	if data, err := os.ReadFile(filepath.Join(rec.ResultDir, "task-package.json")); err == nil {
		inputs["task-package.json"] = string(data)
	}
	for name, content := range inputs {
		if err := os.WriteFile(filepath.Join(reviewDir, name), []byte(content), 0o644); err != nil {
			return nil, fmt.Errorf("writing review input %s: %w", name, err)
		}
	}

	runID := opts.RunID
	if runID == "" {
		runID = NewRunID()
	}
	env, err := BuildWorkerEnv(os.Environ(), opts.RepoRoot, reviewID, scratch, reviewDir, false)
	if err != nil {
		return nil, err
	}
	env.Vars = append(env.Vars, "POST_WORKER_RUN_ID="+runID)

	prompt := renderReviewPrompt(opts.TaskID, reviewDir, gate)
	system := renderReviewSystem(opts.TaskID)
	guardPath, err := WriteGuardFiles(reviewDir, GuardOpts{
		RepoRoot: opts.RepoRoot, TaskID: reviewID, Worktree: scratch,
		ResultDir: reviewDir, DockerGrant: false,
	})
	if err != nil {
		return nil, err
	}
	_ = guardPath
	settingsPath := filepath.Join(reviewDir, "worker-settings.json")
	if err := os.WriteFile(filepath.Join(reviewDir, "prompt.md"), []byte(prompt), 0o644); err != nil {
		return nil, err
	}
	systemPath := filepath.Join(reviewDir, "system.md")
	if err := os.WriteFile(systemPath, []byte(system), 0o644); err != nil {
		return nil, err
	}

	reviewSchema, err := resultSchemaAtForCLI(filepath.Join(opts.RepoRoot, "specs", "orchestrator", "review-verdict.schema.json"))
	if err != nil {
		return nil, err
	}
	spawnOpts := &SpawnOpts{
		RepoRoot: opts.RepoRoot, TaskID: reviewID, Model: opts.Model,
		MaxTurns: opts.MaxTurns, MaxBudgetUSD: opts.MaxBudgetUSD,
		Timeout: opts.Timeout, RunID: runID, ClaudeBin: claudeBin, Bare: true,
	}
	sessionID, err := newUUID()
	if err != nil {
		return nil, err
	}
	args, err := claudeArgs(spawnOpts, sessionID, settingsPath, systemPath, prompt, reviewSchema)
	if err != nil {
		return nil, err
	}

	// Authoritative record for the review run — including the fingerprint the
	// collect will judge against.
	reviewInputs := &GateInputs{
		TaskID:        reviewID,
		RunID:         runID,
		SessionID:     sessionID,
		BaselineSHA:   gate.BaselineSHA,
		Branch:        rec.Branch,
		Worktree:      scratch,
		ResultDir:     reviewDir,
		LogPath:       filepath.Join(reviewDir, "worker.log"),
		AllowedScope:  []string{},
		ReviewDiffSHA: fp,
	}
	if err := WriteGateInputs(opts.RepoRoot, reviewID, reviewInputs, reviewDir); err != nil {
		return nil, fmt.Errorf("recording authoritative review inputs: %w", err)
	}

	// Reaper (same wrapper as a task Worker; arg 5 is the authoritative exit
	// status copy).
	logPath := filepath.Join(reviewDir, "worker.log")
	pidFile := filepath.Join(reviewDir, "claude.pid")
	statusFile := filepath.Join(reviewDir, "exit.status")
	var workerCmd []string
	if opts.Timeout > 0 {
		workerCmd = wrapTimeout(opts.Timeout, claudeBin, args)
	} else {
		workerCmd = append([]string{claudeBin}, args...)
	}
	cmdArgs := append([]string{filepath.Join(reviewDir, "run-worker.sh"), scratch, logPath, pidFile, statusFile, authoritativeExitStatusPath(opts.RepoRoot, reviewID)}, workerCmd...)
	cmd := exec.Command("bash", cmdArgs...)
	cmd.Dir = opts.RepoRoot
	cmd.Env = env.Vars
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	listenersBefore, err := ListenerSnapshot()
	if err != nil {
		return nil, fmt.Errorf("snapshotting listening sockets for the review residue check: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting the review reaper wrapper: %w", err)
	}
	pidCh := make(chan int, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		if sc.Scan() {
			var pid int
			if _, err := fmt.Sscanf(sc.Text(), "%d", &pid); err == nil {
				pidCh <- pid
			}
		}
		close(pidCh)
	}()
	var reviewPID int
	select {
	case reviewPID = <-pidCh:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		return nil, fmt.Errorf("the review reaper did not report a pid within 10s — review spawn aborted, nothing was recorded")
	}
	if reviewPID <= 0 {
		cmd.Process.Kill()
		return nil, fmt.Errorf("the review reaper reported an invalid pid — review spawn aborted, nothing was recorded")
	}
	sessionLeaderPID := cmd.Process.Pid
	_ = cmd.Process.Release()

	for _, pid := range []int{reviewPID, sessionLeaderPID} {
		environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		if err != nil {
			killWorker(reviewPID)
			return nil, fmt.Errorf("review post-spawn environment assertion: reading /proc/%d/environ: %w — spawn aborted and the Reviewer killed", pid, err)
		}
		if leak := assertCleanWorkerEnv(environ); leak != "" {
			killWorker(reviewPID)
			return nil, fmt.Errorf("review post-spawn environment assertion: %s is present in the environment of pid %d — the Reviewer must not hold remote credentials, spawn aborted and the Reviewer killed", leak, pid)
		}
	}

	startedAt := time.Now().UTC().Format(time.RFC3339)
	startTime, err := procStartTime(reviewPID)
	if err != nil {
		killWorker(reviewPID)
		return nil, fmt.Errorf("reading /proc starttime for the Reviewer pid %d: %w", reviewPID, err)
	}
	claudeVersion, err := gitOutput2(claudeBin, "--version")
	if err != nil {
		killWorker(reviewPID)
		return nil, fmt.Errorf("reading claude version for the Reviewer: %w", err)
	}
	// The Reviewer's authoritative record gets the process identity the early
	// WriteGateInputs could not know; review collect compares the registry
	// against these values.
	if err := FinalizeGateInputs(opts.RepoRoot, reviewID, runID, sessionID, reviewPID, startTime, sessionLeaderPID, startedAt, listenersBefore); err != nil {
		killWorker(reviewPID)
		return nil, fmt.Errorf("finalizing the authoritative review inputs: %w", err)
	}
	if err := SaveRegistry(opts.RepoRoot, &WorkerRecord{
		TaskID: reviewID, RunID: runID, SessionID: sessionID,
		ClaudeVersion: claudeVersion, Model: opts.Model,
		MaxTurns: opts.MaxTurns, MaxBudgetUSD: opts.MaxBudgetUSD,
		Timeout: opts.Timeout.String(),
		PID:     reviewPID, StartTime: startTime,
		Worktree: scratch, Branch: rec.Branch, BaselineSHA: gate.BaselineSHA,
		LogPath: logPath, ResultDir: reviewDir, StartedAt: startedAt,
		SessionLeaderPID: sessionLeaderPID, ListenersBefore: listenersBefore,
	}); err != nil {
		killWorker(reviewPID)
		return nil, fmt.Errorf("writing the review registry: %w", err)
	}
	return &SpawnResult{
		TaskID: reviewID, RunID: runID, SessionID: sessionID, PID: reviewPID,
		Worktree: scratch, Branch: rec.Branch, BaselineSHA: gate.BaselineSHA,
		RegistryPath: registryPath(opts.RepoRoot, reviewID),
		LogPath:      logPath, ClaudeVersion: claudeVersion, StartedAt: startedAt,
	}, nil
}

// CollectReview verifies a finished Review Worker: exit 0, no residue, the
// reviewed worktree unchanged (spawn fingerprint), the verdict schema-valid
// and addressed to the reviewed task. The verdict is copied into the gate
// evidence dir and a ReviewRecord is written — the merge gate reads it.
func CollectReview(opts *CollectOpts) (*ReviewCollectReport, error) {
	reviewID := ReviewTaskID(opts.TaskID)
	repoRoot := opts.RepoRoot
	if _, err := DiscoverWorkers(repoRoot); err != nil {
		return nil, err
	}
	rec, err := LoadRegistry(repoRoot, reviewID)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("no recorded Review Worker for %s — nothing to collect", opts.TaskID)
	}
	if rec.ExitStatus == nil {
		return nil, fmt.Errorf("the Review Worker for %s has no recorded exit status (still running or stale)", opts.TaskID)
	}
	report := &ReviewCollectReport{TaskID: opts.TaskID, Checks: []CollectCheck{}}
	fail := func(name, detail string) {
		report.Checks = append(report.Checks, CollectCheck{Name: name, Status: "failed", Detail: detail})
		report.Reasons = append(report.Reasons, name+": "+detail)
	}
	pass := func(name, detail string) {
		report.Checks = append(report.Checks, CollectCheck{Name: name, Status: "passed", Detail: detail})
	}

	if *rec.ExitStatus == 0 {
		pass("review-exit", "exit status 0")
	} else {
		fail("review-exit", fmt.Sprintf("exit status %d — the Reviewer did not complete", *rec.ExitStatus))
		report.Status = "failed"
		return report, nil
	}
	residue, _, err := scanResidue(rec)
	if err != nil {
		return nil, fmt.Errorf("scanning for Review Worker residue: %w", err)
	}
	if len(residue) > 0 {
		parts := make([]string, 0, len(residue))
		for _, p := range residue {
			parts = append(parts, fmt.Sprintf("pid %d %s", p.PID, p.Cmdline))
		}
		fail("review-residue", fmt.Sprintf("%d process(es) the Reviewer started are still running: %s", len(residue), strings.Join(parts, "; ")))
	} else {
		pass("review-residue", "no live processes in the Reviewer's session")
	}

	// The reviewed code must be exactly the code the review started from.
	taskRec, err := LoadRegistry(repoRoot, opts.TaskID)
	if err != nil {
		return nil, err
	}
	if taskRec != nil {
		gate, err := LoadGateInputs(repoRoot, opts.TaskID)
		if err != nil {
			return nil, err
		}
		revGate, err := LoadGateInputs(repoRoot, reviewID)
		if err != nil {
			return nil, err
		}
		// The Reviewer's own registry must match its authoritative record —
		// the Reviewer can write its registry.json like any Worker, and a
		// rewritten pid/listener baseline would blind the residue check.
		if revGate != nil {
			for _, t := range VerifyGateInputs(repoRoot, rec, revGate) {
				fail("review-gate-inputs", fmt.Sprintf("%s: %s", t.What, t.Detail))
			}
		} else {
			fail("review-gate-inputs", "no authoritative spawn record exists for the Review Worker — refusing to trust an unanchored review")
			report.Status = "rejected"
			return report, nil
		}
		baseline := ""
		if gate != nil {
			baseline = gate.BaselineSHA
		} else {
			baseline = taskRec.BaselineSHA
		}
		fp, err := taskWorktreeFingerprint(taskRec, baseline)
		if err != nil {
			return nil, err
		}
		if revGate != nil && revGate.ReviewDiffSHA != "" && fp != revGate.ReviewDiffSHA {
			fail("review-code-unchanged", "the reviewed worktree changed during the review — the verdict describes different code; review again")
		} else {
			pass("review-code-unchanged", "the reviewed worktree matches the spawn-time fingerprint")
		}
	}

	// The verdict document: schema + task id + verdict recorded as evidence.
	//
	// The Reviewer's own file is preferred, but its verdict is also carried by
	// the harness: --output-format stream-json ends with a `result` event whose
	// payload is the final structured output, already validated against the
	// same schema at the end of the session. The prompt asks for both, and a
	// Reviewer that follows half of it — T0102's wrote an approving verdict
	// into StructuredOutput and never touched the file — would otherwise lose
	// a valid verdict and force a re-review of work that was fine. Asking in
	// prose is not a mechanism; this is.
	verdictPath := filepath.Join(rec.ResultDir, "RESULT.json")
	if _, err := os.Stat(verdictPath); os.IsNotExist(err) {
		if recovered, ok, rerr := verdictFromSessionLog(rec.LogPath); rerr != nil {
			return nil, fmt.Errorf("recovering the verdict from the Reviewer's session log: %w", rerr)
		} else if ok {
			if werr := os.WriteFile(verdictPath, recovered, 0o644); werr != nil {
				return nil, fmt.Errorf("recording the recovered verdict: %w", werr)
			}
		}
	}
	verdictSchema := filepath.Join(repoRoot, "specs", "orchestrator", "review-verdict.schema.json")
	if err := ValidateWorkerResultFile(verdictSchema, verdictPath); err != nil {
		fail("review-verdict-schema", err.Error())
		report.Status = "rejected"
		return report, nil
	}
	data, err := os.ReadFile(verdictPath)
	if err != nil {
		return nil, fmt.Errorf("reading the verdict: %w", err)
	}
	var verdict ReviewVerdict
	if err := json.Unmarshal(data, &verdict); err != nil {
		fail("review-verdict-parse", err.Error())
		report.Status = "rejected"
		return report, nil
	}
	if verdict.TaskID != opts.TaskID {
		fail("review-verdict-task", fmt.Sprintf("verdict task_id is %q, want %q", verdict.TaskID, opts.TaskID))
		report.Status = "rejected"
		return report, nil
	}
	report.Verdict = &verdict
	blocking, major := 0, 0
	for _, f := range verdict.Findings {
		if f.Severity == "blocking" {
			blocking++
		}
		if f.Severity == "major" {
			major++
		}
	}
	pass("review-verdict", fmt.Sprintf("verdict %q (%d blocking, %d major finding(s))", verdict.Verdict, blocking, major))

	if len(report.Reasons) > 0 {
		report.Status = "rejected"
		return report, nil
	}
	report.Status = "ok"
	// Record the verdict as gate evidence: a copy of the document plus the
	// ReviewRecord the merge gate reads.
	runID := opts.RunID
	if runID == "" {
		runID = NewRunID()
	}
	if err := writeFileAtomic(filepath.Join(GatesDir(repoRoot, opts.TaskID), "verdict-"+runID+".json"), data); err != nil {
		return nil, fmt.Errorf("storing the verdict evidence: %w", err)
	}
	if _, err := WriteRecord(repoRoot, opts.TaskID, RecordReview, runID, &ReviewRecord{
		recordMeta:        recordMeta{RecordType: RecordReview, TaskID: opts.TaskID, RunID: runID, At: nowRFC3339()},
		Verdict:           verdict.Verdict,
		Summary:           verdict.Summary,
		BlockingFindings:  blocking,
		MajorFindings:     major,
		VerdictPath:       verdictPath,
		ReviewerSessionID: rec.SessionID,
	}); err != nil {
		return nil, err
	}
	return report, nil
}

// taskWorktreeFingerprint hashes everything the review judges: the tracked
// diff against the baseline plus every untracked file's name and content. A
// change of either during the review invalidates the verdict.
func taskWorktreeFingerprint(rec *WorkerRecord, baseline string) (string, error) {
	h := sha256.New()
	diff, err := gitOutput(rec.Worktree, "diff", baseline, "--")
	if err != nil {
		return "", fmt.Errorf("diffing the worktree for the review fingerprint: %w", err)
	}
	h.Write([]byte(diff))
	untracked, err := gitOutput(rec.Worktree, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return "", fmt.Errorf("listing untracked files for the review fingerprint: %w", err)
	}
	for _, p := range strings.Split(untracked, "\n") {
		if p == "" {
			continue
		}
		h.Write([]byte(p + "\n"))
		data, err := os.ReadFile(filepath.Join(rec.Worktree, p))
		if err == nil {
			h.Write(data)
		} else {
			h.Write([]byte("(unreadable)"))
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// renderReviewPrompt builds the Reviewer's task: read the inputs in its
// result dir and produce a verdict document.
func renderReviewPrompt(taskID, reviewDir string, gate *GateInputs) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Independent review of task %s\n\n", taskID)
	b.WriteString("You are an independent Review Worker in the POST development system.\n")
	b.WriteString("You have NO write scope and NO Git control-plane permissions. Your\n")
	b.WriteString("product is a review verdict, nothing else.\n\n")
	b.WriteString("Read these files in your result dir (all paths are inside it):\n\n")
	fmt.Fprintf(&b, "  %s\n", filepath.Join(reviewDir, "diff.txt"))
	fmt.Fprintf(&b, "  %s\n", filepath.Join(reviewDir, "task-package.json"))
	fmt.Fprintf(&b, "  %s\n\n", filepath.Join(reviewDir, "collect-report.json"))
	// Say what the diff actually IS. Calling it "the complete change" was true
	// while the baseline was the spawn-point commit and stops being true the
	// moment a baseline is advanced: after a rework onto a newer main the
	// baseline is the merge commit, so the diff is only what has changed
	// SINCE it, and the task's earlier work is already inside both the
	// baseline and the previous collect. A review input that describes itself
	// inaccurately is the defect this whole system exists to prevent, and the
	// Reviewer that caught the previous instance had to reconcile the file
	// count against the collect report to notice.
	fmt.Fprintf(&b, "The diff in diff.txt is the change for task %s measured against baseline\n%s. Read it as exactly that: if the baseline is a merge or a rebase point\nrather than the commit the task started from, this is the change since that\nbaseline, NOT the task's whole contribution — the earlier part is already in\nthe baseline and was verified by the collect the baseline came from. The\nworktree itself holds the complete state; read it when the diff alone cannot\nanswer a question. The collect report lists the mechanical checks that\npassed, and its file list is the authoritative account of what this round\nchanged. Your job is the independent human-level review the machines cannot\ndo:\n\n", taskID, gate.BaselineSHA)
	b.WriteString("- Does the diff actually satisfy every acceptance criterion in the task package?\n")
	b.WriteString("- Do the required tests exist, and does the evidence support them?\n")
	b.WriteString("- Are there correctness defects, security problems, scope violations,\n  weakened tests, or product-semantics changes the task did not authorize?\n")
	b.WriteString("- Is anything in the RESULT.json evidence contradicted by the diff?\n\n")
	b.WriteString("Produce a verdict document with verdict approve (the work satisfies the\ncontract) or request_changes (it does not), with findings — each carrying a\nseverity (blocking / major / minor / nit), the file and line where known, and\nthe finding itself. A blocking finding means request_changes. Risks are\nforward-looking concerns that do not block.\n\n")
	b.WriteString("Your FINAL message must be the verdict document. This session runs with\n--json-schema: the final output is mechanically validated against\nspecs/orchestrator/review-verdict.schema.json, and the file you write to\nRESULT.json is re-validated at collection — write the same document to both.\n")
	return b.String()
}

// renderReviewSystem renders the Reviewer's system prompt: read-only, no
// control plane, verdict only.
func renderReviewSystem(taskID string) string {
	return fmt.Sprintf(`# You are an independent Review Worker for task %s in the POST development system.

## Hard rules

- Read-only: you have NO write scope. Your only output is the verdict document.
- No Git control-plane: never commit, push, merge, rebase, tag, branch, or
  mutate worktrees, refs, remotes or git config.
- No remote credentials; no network actions.
- Do not modify the reviewed code, its worktree, or any repo file — the
  reviewed-code fingerprint is verified at collection and a changed worktree
  invalidates your verdict.
- Be adversarial: try to make the reviewed change fail. A verdict of approve
  is your professional judgement that the diff satisfies the task contract,
  not a rubber stamp.
- If you cannot decide, request_changes with the precise open questions.

Your verdict is recorded as gate evidence: an approving verdict is required
for the merge gate (G4).
`, taskID)
}

// verdictFromSessionLog recovers the final structured output from a
// stream-json session log.
//
// The last `result` event carries `result`, the harness's own serialisation of
// the final message — the same document the CLI validated against
// review-verdict.schema.json before ending the session. It is the Reviewer's
// verdict, captured by the harness rather than typed into a file, which makes
// it the more reliable of the two copies rather than the fallback for a
// broken one.
func verdictFromSessionLog(logPath string) ([]byte, bool, error) {
	f, err := os.Open(logPath)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	var last string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev struct {
			Type   string `json:"type"`
			Result string `json:"result"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Type == "result" && ev.Result != "" {
			last = ev.Result
		}
	}
	if err := sc.Err(); err != nil {
		return nil, false, err
	}
	if last == "" {
		return nil, false, nil
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(last), &doc); err != nil {
		return nil, false, nil
	}
	if _, ok := doc["verdict"]; !ok {
		return nil, false, nil
	}
	return []byte(last), true, nil
}
