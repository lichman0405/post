package devorchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The gate executor (T0012 requirement: "G1/G2/G3/G4 as task metadata and an
// executor, each recording commands, exit status and output"). G2 runs the
// CI jobs' EXACT steps — every step of the six jobs, one subprocess per step,
// exit code and combined output recorded per step; a similar-looking subset
// (the defect that let a red PR merge) is impossible because the job/step
// list comes from specs/orchestrator/gates.json, which a unit test keeps in
// sync with .github/workflows/ci.yml verbatim. G4 does not run anything: it
// asserts the latest recorded G2 run covered every required job green, plus
// G1 (latest collect ok), G3 (task override), and the review verdict — the
// merge refuses while any is red or missing.

// GateRunOpts parametrizes one gate execution.
type GateRunOpts struct {
	RepoRoot  string
	GatesPath string // override (e2e); empty = specs/orchestrator/gates.json
	TaskID    string
	Gate      string // G2 or G3
	RunID     string
	JobEnv    map[string]string // extra environment for every job (e.g. G3 database URLs)
}

// GateRunResult is the outcome of one executed gate.
type GateRunResult struct {
	TaskID     string          `json:"task_id"`
	Gate       string          `json:"gate"`
	RunID      string          `json:"run_id"`
	Status     string          `json:"status"` // passed | failed
	Jobs       []GateJobResult `json:"jobs"`
	At         string          `json:"at"`
	EndedAt    string          `json:"ended_at"`
	RecordPath string          `json:"record_path"`
}

// RunGate executes the jobs of one gate, records every step, and writes the
// GateRunRecord evidence file. It returns the result for the caller; a
// non-nil error means the gate could not be executed at all (missing spec,
// unknown gate), while a failed job is a passed-through result with
// Status "failed" — callers decide what a red gate refuses.
func RunGate(opts *GateRunOpts) (*GateRunResult, error) {
	if opts == nil || opts.RepoRoot == "" || opts.TaskID == "" || opts.Gate == "" {
		return nil, fmt.Errorf("gate run requires repo root, task id and gate name")
	}
	gatesPath := opts.GatesPath
	if gatesPath == "" {
		gatesPath = DefaultGatesPath
	}
	spec, err := gateSpecAt(opts.RepoRoot, gatesPath)
	if err != nil {
		return nil, err
	}
	// The tree these steps run in: current main PLUS this task's complete
	// change — the same thing a pull request is, and the only thing whose
	// compilability matters, because it is what merging produces. Running in
	// the task's own worktree (the previous behaviour) reported green for
	// trees that cannot be composed with main at all: five separate collisions
	// across P1, every one of which git called MERGEABLE.
	workDir, cleanupTree, err := prepareIntegrationTree(opts.RepoRoot, opts.TaskID)
	if err != nil {
		return nil, err
	}
	defer cleanupTree()
	if workDir == "" {
		workDir = opts.RepoRoot // no Worker worktree (a fixture): nothing to compose
	}
	if opts.Gate != "G2" && opts.Gate != "G3" {
		return nil, fmt.Errorf("gate %s is not executable — G1 runs at collect and G4 asserts against G2 records (run `rddev git commit`/`rddev pr merge`, which run the G4 assertion)", opts.Gate)
	}
	jobs, err := spec.JobsForGate(opts.Gate, opts.TaskID)
	if err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return nil, fmt.Errorf("gate %s declares no jobs for task %s", opts.Gate, opts.TaskID)
	}
	runID := opts.RunID
	if runID == "" {
		runID = NewRunID()
	}
	// A gate run's record is named gate-run-<runID>.json, so two gates run by
	// ONE command under the same caller-supplied run id collide on the
	// filename — and the second silently overwrites the first. That is not
	// hypothetical: `task accept` runs G2 and then G3 with the same run id, so
	// every task with a G3 override lost its G2 record the moment G3 ran, and
	// the merge gate then refused with "no G2 gate-run record exists" for a
	// task whose G2 had just passed. The gate belongs in the identity of its
	// own run.
	runID = runID + "-" + strings.ToLower(opts.Gate)
	res := &GateRunResult{TaskID: opts.TaskID, Gate: opts.Gate, RunID: runID, At: nowRFC3339(), Status: "passed"}
	overallFailed := false
	for _, jobName := range jobs {
		job, ok := spec.Jobs[jobName]
		if !ok {
			return nil, fmt.Errorf("gate %s names job %q but it is not defined in %s", opts.Gate, jobName, gatesPath)
		}
		jr := GateJobResult{Job: jobName, Status: "passed"}
		jobFailed := false
		for i, step := range job.Steps {
			if jobFailed {
				jr.Steps = append(jr.Steps, GateStepResult{Index: i, Run: step.Run, Exit: -1, Skipped: true})
				continue
			}
			sr, err := runGateStep(workDir, opts.RepoRoot, opts.TaskID, runID, jobName, i, step, nil, opts.JobEnv)
			if err != nil {
				return nil, fmt.Errorf("gate %s job %s step %d: %w", opts.Gate, jobName, i, err)
			}
			jr.Steps = append(jr.Steps, sr)
			if sr.Exit != 0 {
				jr.Status = "failed"
				jobFailed = true
				overallFailed = true
			}
		}
		res.Jobs = append(res.Jobs, jr)
	}
	res.EndedAt = nowRFC3339()
	if overallFailed {
		res.Status = "failed"
	}
	rec := &GateRunRecord{
		recordMeta: recordMeta{RecordType: RecordGateRun, TaskID: opts.TaskID, RunID: runID, At: res.At},
		Gate:       opts.Gate,
		Status:     res.Status,
		Jobs:       res.Jobs,
		EndedAt:    res.EndedAt,
		Note:       fmt.Sprintf("jobs from %s (CI's exact steps)", gatesPath),
	}
	path, err := WriteRecord(opts.RepoRoot, opts.TaskID, RecordGateRun, runID, rec)
	if err != nil {
		return nil, err
	}
	res.RecordPath = path
	return res, nil
}

// runGateStep executes one step of a CI job the way GitHub Actions does
// (bash --noprofile --norc -eo pipefail -c), captures stdout+stderr into a
// per-step log, and returns the exit code. The step's own env merges over
// the job env, which merges over the inherited environment.
func runGateStep(workDir, repoRoot, taskID, runID, jobName string, index int, step GateStep, jobEnv, extraEnv map[string]string) (GateStepResult, error) {
	sr := GateStepResult{Index: index, Run: step.Run, StartedAt: nowRFC3339()}
	outDir := filepath.Join(GateOutputDir(repoRoot, taskID, runID), jobName)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return sr, err
	}
	outPath := filepath.Join(outDir, fmt.Sprintf("%02d.log", index))
	logf, err := os.Create(outPath)
	if err != nil {
		return sr, err
	}
	fmt.Fprintf(logf, "$ %s\n", step.Run)
	cmd := exec.Command("bash", "--noprofile", "--norc", "-eo", "pipefail", "-c", step.Run)
	// The cwd is chosen once per gate run by prepareIntegrationTree: current
	// main plus the task's complete change. See RunGate.
	cmd.Dir = workDir
	cmd.Env = os.Environ()
	for k, v := range jobEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	for k, v := range extraEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	for k, v := range step.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout = logf
	cmd.Stderr = logf
	err = cmd.Run()
	logf.Close()
	sr.EndedAt = nowRFC3339()
	sr.OutputFile = outPath
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			sr.Exit = ee.ExitCode()
			return sr, nil // a failing step is a gate result, not an executor error
		}
		return sr, fmt.Errorf("running step: %w", err)
	}
	sr.Exit = 0
	return sr, nil
}

// LatestGateRunRecord returns the newest gate-run record for one specific
// gate (G2 or G3); ok=false when that gate has never run.
func LatestGateRunRecord(repoRoot, taskID, gate string) (GateRunRecord, bool, error) {
	entries, err := os.ReadDir(GatesDir(repoRoot, taskID))
	if os.IsNotExist(err) {
		return GateRunRecord{}, false, nil
	}
	if err != nil {
		return GateRunRecord{}, false, fmt.Errorf("scanning gate records for %s: %w", taskID, err)
	}
	var best GateRunRecord
	var bestAt string
	var bestName string
	found := false
	prefix := RecordGateRun + "-"
	var bestMtime int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(GatesDir(repoRoot, taskID), e.Name()))
		if err != nil {
			return GateRunRecord{}, false, err
		}
		var rec GateRunRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			continue
		}
		if rec.Gate != gate {
			continue
		}
		// Same tie-break as LatestRecord: equal At resolves by mtime (later
		// write wins), then name — never by random run-id order.
		info, _ := e.Info()
		mtime := int64(0)
		if info != nil {
			mtime = info.ModTime().UnixNano()
		}
		if !found || rec.At > bestAt || (rec.At == bestAt && mtime > bestMtime) || (rec.At == bestAt && mtime == bestMtime && e.Name() > bestName) {
			best = rec
			bestAt = rec.At
			bestMtime = mtime
			bestName = e.Name()
			found = true
		}
	}
	return best, found, nil
}

// Gate4Result is the merge-gate assertion: what was checked and why the
// merge is refused (refused == Status "failed", with every reason listed).
type Gate4Result struct {
	TaskID  string   `json:"task_id"`
	Status  string   `json:"status"` // passed | failed
	Checks  []string `json:"checks"`
	Reasons []string `json:"reasons,omitempty"`
}

// CheckMergeGate runs the G4 assertion for one task: the latest G2 record
// must exist, cover every required CI job with status passed, and be at/after
// the latest collect record (G1 ok — the G2 evidence judged the collected
// code); G3 must have a passed run when the task defines G3 jobs; the review
// verdict must be approve when review is required for merge. Every refusal
// reason is enumerated — the tooling refuses while any is present, and the
// reasons are the rejection evidence.
func CheckMergeGate(repoRoot, gatesPath, taskID string) (*Gate4Result, error) {
	if gatesPath == "" {
		gatesPath = DefaultGatesPath
	}
	spec, err := gateSpecAt(repoRoot, gatesPath)
	if err != nil {
		return nil, err
	}
	res := &Gate4Result{TaskID: taskID, Status: "passed"}
	fail := func(reason string) {
		res.Status = "failed"
		res.Reasons = append(res.Reasons, reason)
	}

	// G4 asserts_jobs: every required CI job, green in the latest G2 record.
	required := spec.RequiredJobs
	res.Checks = append(res.Checks, fmt.Sprintf("G4 asserts_jobs = %s (the six required CI jobs)", strings.Join(required, ", ")))

	g2, g2ok, err := LatestGateRunRecord(repoRoot, taskID, "G2")
	if err != nil {
		return nil, err
	}
	if !g2ok {
		fail("no G2 gate-run record exists — the Supervisor has not run CI's exact steps for this task (rddev gate run G2 " + taskID + ")")
		return res, nil
	}
	got := map[string]string{}
	for _, j := range g2.Jobs {
		got[j.Job] = j.Status
	}
	var missing, red []string
	for _, j := range required {
		st, present := got[j]
		switch {
		case !present:
			missing = append(missing, j)
		case st != "passed":
			red = append(red, j+" ("+st+")")
		}
	}
	if len(missing) > 0 || len(red) > 0 {
		fail(fmt.Sprintf("G2 record %s is not green for all required jobs — missing: %s; red: %s — a subset-G2 (the defect a merge must never pass) does not satisfy the merge gate",
			g2.RunID, strings.Join(missing, ", "), strings.Join(red, ", ")))
	} else {
		res.Checks = append(res.Checks, fmt.Sprintf("G2 record %s covers all %d required jobs, all passed", g2.RunID, len(required)))
	}

	// G1: the latest collect must be ok and not newer than the G2 evidence
	// (a collect after the G2 run means the code moved after G2 judged it).
	coll, cok, err := LatestRecord[CollectRecord](repoRoot, taskID, RecordCollect)
	if err != nil {
		return nil, err
	}
	if !cok {
		fail("no collect record exists — G1 has never passed for this task")
	} else if coll.Status != "ok" {
		fail(fmt.Sprintf("the latest collect (%s) is %s, not ok — G1 is red", coll.RunID, coll.Status))
	} else {
		res.Checks = append(res.Checks, fmt.Sprintf("G1 collect %s ok", coll.RunID))
		if coll.At > g2.At {
			fail(fmt.Sprintf("the latest collect (%s, %s) is newer than the G2 run (%s, %s) — G2 did not judge the code as collected; re-run G2", coll.RunID, coll.At, g2.RunID, g2.At))
		} else {
			res.Checks = append(res.Checks, fmt.Sprintf("G2 evidence (%s) is at/after the latest collect (%s)", g2.At, coll.At))
		}
	}

	// G3: required only when the task defines G3 jobs.
	g3Jobs, err := spec.JobsForGate("G3", taskID)
	if err != nil {
		return nil, err
	}
	if len(g3Jobs) == 0 {
		res.Checks = append(res.Checks, "G3 not_required (no g3_jobs override for this task)")
	} else {
		g3, g3ok, err := LatestGateRunRecord(repoRoot, taskID, "G3")
		if err != nil {
			return nil, err
		}
		g3Passed := false
		if g3ok && g3.Status == "passed" {
			covered := true
			for _, j := range g3Jobs {
				found := false
				for _, jr := range g3.Jobs {
					if jr.Job == j && jr.Status == "passed" {
						found = true
					}
				}
				if !found {
					covered = false
				}
			}
			g3Passed = covered
		}
		if !g3Passed {
			fail(fmt.Sprintf("G3 requires jobs %s but no green G3 run covers them (rddev gate run G3 %s)", strings.Join(g3Jobs, ", "), taskID))
		} else {
			res.Checks = append(res.Checks, fmt.Sprintf("G3 jobs %s green in run %s", strings.Join(g3Jobs, ", "), g3.RunID))
		}
	}

	// Review: an approving verdict is required for merge unless the task
	// overrides it.
	if spec.ReviewRequiredForMerge(taskID) {
		rv, rok, err := LatestRecord[ReviewRecord](repoRoot, taskID, RecordReview)
		if err != nil {
			return nil, err
		}
		if !rok {
			fail("review is required for merge but no review verdict exists (rddev review spawn " + taskID + ", then rddev review collect " + taskID + ")")
		} else if rv.Verdict != "approve" {
			fail(fmt.Sprintf("the latest review verdict is %q with %d blocking finding(s) — an approving Review Worker verdict is required for merge", rv.Verdict, rv.BlockingFindings))
		} else if cok && rv.At < coll.At {
			// Freshness, the same rule the G2 checks above apply: a verdict
			// describes the diff that existed when it was written. Approving
			// code that has since changed — a rework, a Supervisor glue edit —
			// must not satisfy the merge gate, or the verdict becomes evidence
			// for a tree nobody reviewed.
			fail(fmt.Sprintf("the review verdict (%s, %s) is older than the latest collect (%s, %s) — it judged a different tree; re-review the current diff (rddev review spawn %s)",
				rv.RunID, rv.At, coll.RunID, coll.At, taskID))
		} else if want, werr := currentCodeIdentity(repoRoot, taskID); werr != nil {
			fail(fmt.Sprintf("could not recompute the code identity the verdict must be bound to: %v", werr))
		} else if want == "" {
			// No code state to bind to (a fixture, or a task that was never
			// spawned). Production always has a worktree, so this branch can
			// never be why a stale verdict passes.
			res.Checks = append(res.Checks, fmt.Sprintf("review verdict approve (%s); no worktree to bind to, binding not asserted", rv.RunID))
		} else if rv.DiffSHA == "" {
			fail("the review verdict carries no diff identity — it cannot be bound to the code it judged. Re-review (" + "rddev review spawn " + taskID + ").")
		} else if rv.DiffSHA != want {
			fail("the review verdict is about a DIFFERENT code state: it was written for code identity " + rv.DiffSHA[:12] + "…, the tree is now " + want[:12] + "… (a rebase, a baseline advance, a merge repair, or any edit since the review). A verdict must not outlive the code it judged — re-review the current diff.")
		} else {
			res.Checks = append(res.Checks, fmt.Sprintf("review verdict approve (%s), bound to code identity %s…", rv.RunID, rv.DiffSHA[:12]))
		}
	} else {
		res.Checks = append(res.Checks, "review not required for merge (task override)")
	}
	return res, nil
}

// AcceptGateStatus builds the gate status map for an accept decision: G1 from
// the latest collect, G2/G3 from the latest runs per gate, G4 not relevant.
func AcceptGateStatus(repoRoot, gatesPath, taskID string) (map[string]string, error) {
	status := map[string]string{}
	coll, ok, err := LatestRecord[CollectRecord](repoRoot, taskID, RecordCollect)
	if err != nil {
		return nil, err
	}
	if ok && coll.Status == "ok" {
		status["G1"] = "passed"
	} else {
		status["G1"] = "failed"
	}
	g2, ok, err := LatestGateRunRecord(repoRoot, taskID, "G2")
	if err != nil {
		return nil, err
	}
	if ok && g2.Status == "passed" {
		status["G2"] = "passed"
	} else {
		status["G2"] = "failed"
	}
	if gatesPath == "" {
		gatesPath = DefaultGatesPath
	}
	spec, err := gateSpecAt(repoRoot, gatesPath)
	if err != nil {
		return nil, err
	}
	if jobs, err := spec.JobsForGate("G3", taskID); err != nil {
		return nil, err
	} else if len(jobs) == 0 {
		status["G3"] = "not_required"
	} else {
		g3, ok, err := LatestGateRunRecord(repoRoot, taskID, "G3")
		if err != nil {
			return nil, err
		}
		if ok && g3.Status == "passed" {
			status["G3"] = "passed"
		} else {
			status["G3"] = "failed"
		}
	}
	status["G4"] = "not_required"
	return status, nil
}

// EnsureG2Green returns a green G2 run for the task: a fresh all-green G2
// record whose evidence is at/after the latest collect is reused (an
// idempotent accept must not re-run six CI jobs when the evidence is already
// green and current); otherwise the gate is executed now. Callers use this so
// `rddev task accept` never accepts on a subset or stale G2.
func EnsureG2Green(opts *GateRunOpts) (*GateRunResult, error) {
	g2, ok, err := LatestGateRunRecord(opts.RepoRoot, opts.TaskID, "G2")
	if err != nil {
		return nil, err
	}
	coll, collOK, err := LatestRecord[CollectRecord](opts.RepoRoot, opts.TaskID, RecordCollect)
	if err != nil {
		return nil, err
	}
	if ok && g2.Status == "passed" && (!collOK || g2.At >= coll.At) {
		return &GateRunResult{
			TaskID: opts.TaskID, Gate: "G2", RunID: g2.RunID, Status: "passed",
			Jobs: g2.Jobs, At: g2.At, EndedAt: g2.EndedAt,
		}, nil
	}
	opts.Gate = "G2"
	return RunGate(opts)
}

// IntegrationTreeDir is where the tree a gate verifies is built: current main
// plus the task's complete change. Supervisor-owned and gitignored.
func IntegrationTreeDir(repoRoot, taskID string) string {
	return filepath.Join(repoRoot, ".rddev", "runtime", "integration", taskID)
}

// prepareIntegrationTree builds the tree G2 and G3 verify.
//
// The gates must judge what merging would produce, so the tree is current main
// with the task's complete change applied — not the task's own worktree, which
// says nothing about whether the two compose. That distinction cost P1 five
// cross-task collisions (a principal/Principal naming split, an interface that
// kept an old signature, two declarations of domain.AuditEntry for one table),
// each of which git reported as MERGEABLE while the merge did not compile.
//
// Returns dir="" with a no-op cleanup when the task has no Worker worktree
// (fixtures, e2e), so the caller keeps using the repository root.
//
// A change that does not apply to current main is reported as an error naming
// that, rather than as a step failure: it means the branch must be brought up
// to date, which is a different repair from a red test.
func prepareIntegrationTree(repoRoot, taskID string) (string, func(), error) {
	noop := func() {}
	if taskID == "" {
		return "", noop, nil
	}
	rec, err := LoadRegistry(repoRoot, taskID)
	if err != nil || rec == nil || rec.Worktree == "" {
		return "", noop, nil
	}
	if st, err := os.Stat(rec.Worktree); err != nil || !st.IsDir() {
		return "", noop, nil
	}
	dir := IntegrationTreeDir(repoRoot, taskID)
	if _, err := gitOutput(repoRoot, "worktree", "remove", "--force", dir); err != nil {
		// Stale or absent: both are fine, but a leftover directory would make
		// `worktree add` fail, so clear it.
		_ = os.RemoveAll(dir)
	}
	// The tree is "current main plus the task's change", so it has to be built
	// from what main IS rather than from what the clone last heard. A stale ref
	// grades a composition the forge stopped having at the last merge — and it
	// is the same ref a task branch is cut from, so both readers answer the same
	// way (IntegrationTip). A fetch that fails REFUSES the gate: a gate that
	// cannot establish its own premise has no verdict to give.
	tip, err := IntegrationTip(repoRoot)
	if err != nil {
		return "", noop, err
	}
	if _, err := gitOutput(repoRoot, "worktree", "add", "--detach", dir, tip); err != nil {
		return "", noop, fmt.Errorf("creating the integration tree at %s from %s: %w", dir, tip, err)
	}
	cleanup := func() {
		_, _ = gitOutput(repoRoot, "worktree", "remove", "--force", dir)
		_ = os.RemoveAll(dir)
	}
	change, err := taskWorktreeDiff(rec)
	if err != nil {
		cleanup()
		return "", noop, fmt.Errorf("reading the task's change for the integration tree: %w", err)
	}
	if strings.TrimSpace(change) == "" {
		// No change yet: the tree is main, which is a meaningful (if not yet
		// useful) thing to verify.
		return dir, cleanup, nil
	}
	patch := filepath.Join(os.TempDir(), "post-integration-"+taskID+".patch")
	if err := os.WriteFile(patch, []byte(change), 0o600); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("writing the task's change as a patch: %w", err)
	}
	defer os.Remove(patch)
	if _, err := gitOutput(dir, "apply", patch); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("the task's change does not apply to current %s — bring the branch up to date and re-run: %w", DefaultBaseBranch, err)
	}
	return dir, cleanup, nil
}

// currentCodeIdentity recomputes the identity of the code the task's verdict
// must be bound to. Returns "" when there is no worktree to bind to (fixtures,
// e2e, a task that was never spawned), in which case the binding is not
// asserted — but a verdict with NO recorded identity is still refused, so this
// can never be the reason a stale verdict passes.
func currentCodeIdentity(repoRoot, taskID string) (string, error) {
	rec, err := LoadRegistry(repoRoot, taskID)
	if err != nil || rec == nil || rec.Worktree == "" {
		return "", nil
	}
	if st, err := os.Stat(rec.Worktree); err != nil || !st.IsDir() {
		return "", nil
	}
	return codeIdentity(rec)
}
