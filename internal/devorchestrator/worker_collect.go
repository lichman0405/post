package devorchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// `rddev worker collect` closes the Worker boundary (T0011): it re-verifies
// every collection invariant from specs/orchestrator/worker-permissions.yaml
// against real git state instead of trusting the Worker's word — HEAD against
// the recorded baseline, refs against the spawn snapshot, the diff against
// allowed_scope, RESULT.json against worker-result.schema.json (the contract
// the T0010 Worker broke is enforced mechanically here), secret material in
// the diff, and host residue (processes/services the Worker started and did
// not stop; T0007 left a PostgreSQL running and it could have made a
// no-database verification pass spuriously). The report is written to the
// task's result dir before the state transition, so the evidence survives
// even when the state file refuses the transition.

// CollectOpts parametrizes one collection.
type CollectOpts struct {
	RepoRoot  string
	DagPath   string
	StatePath string
	TaskID    string
	RunID     string // state-change run id (default: generated)
}

// CollectCheck is one named invariant check of a collection.
type CollectCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // passed | failed
	Detail string `json:"detail"`
}

// ProcessFinding is one live process attributed to the Worker's session: the
// Worker started it and did not stop it. Collect rejects the result when any
// is found.
type ProcessFinding struct {
	PID     int    `json:"pid"`
	Cmdline string `json:"cmdline"`
	Session int    `json:"session"`
}

// ListenerFinding is a listening TCP socket that appeared during the run but
// cannot be proven to be the Worker's (its owner escaped the Worker's
// session, e.g. a daemonizing service). It is surfaced, not rejected — a
// hard reject here would over-block when a parallel Worker or the Supervisor
// legitimately starts a service concurrently.
type ListenerFinding struct {
	Addr        string `json:"addr"`
	PID         int    `json:"pid,omitempty"`
	Cmdline     string `json:"cmdline,omitempty"`
	Attribution string `json:"attribution"` // started-during-run | pre-existing-owner | no-owner-found
}

// CollectReport is the machine-readable collection outcome.
type CollectReport struct {
	TaskID           string            `json:"task_id"`
	Status           string            `json:"status"` // ok | rejected | failed
	StateTransition  string            `json:"state_transition,omitempty"`
	Checks           []CollectCheck    `json:"checks"`
	FilesChanged     []string          `json:"files_changed"`
	Residue          []ProcessFinding  `json:"residue"`
	ListenerWarnings []ListenerFinding `json:"listener_warnings,omitempty"`
	Reasons          []string          `json:"reasons,omitempty"`
}

// secretPatterns is the conservative, documented set of credential shapes the
// diff is scanned for (worker-permissions.yaml: "no secret material
// introduced"). Findings name the pattern class, never the matched text — a
// rejection must not echo the secret it found.
var secretPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"GitHub classic PAT (ghp_)", regexp.MustCompile(`ghp_[A-Za-z0-9]{20,}`)},
	{"GitHub fine-grained PAT (github_pat_)", regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`)},
	{"AWS access key id (AKIA…)", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"private key block", regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----`)},
	{"Anthropic API key (sk-ant-)", regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`)},
}

// Collect runs the collection pipeline for one finished Worker and advances
// the task state: running -> verification (clean run), running -> rejected
// (invariant violation), running -> worker_failed (nonzero exit). The report
// is returned alongside any operational error so callers can always see what
// was checked.
func Collect(opts *CollectOpts) (*CollectReport, error) {
	if opts == nil || opts.RepoRoot == "" || opts.TaskID == "" {
		return nil, fmt.Errorf("collect requires a repo root and a task id")
	}
	repoRoot, taskID := opts.RepoRoot, opts.TaskID

	rec, err := LoadRegistry(repoRoot, taskID)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("no recorded Worker for %s — nothing to collect", taskID)
	}
	// Reconcile the reaper's exit.status into the registry first (the same
	// disk-derived path `worker list` uses), then re-read.
	if _, err := DiscoverWorkers(repoRoot); err != nil {
		return nil, err
	}
	rec, err = LoadRegistry(repoRoot, taskID)
	if err != nil {
		return nil, err
	}
	if rec.ExitStatus == nil {
		return nil, fmt.Errorf("Worker %s has no recorded exit status (still running or stale) — collect refuses to judge an unfinished run; see `rddev worker list`", taskID)
	}

	report := &CollectReport{
		TaskID:       taskID,
		FilesChanged: []string{},
		Residue:      []ProcessFinding{},
		Checks:       []CollectCheck{},
	}
	fail := func(name, detail string) {
		report.Checks = append(report.Checks, CollectCheck{Name: name, Status: "failed", Detail: detail})
		report.Reasons = append(report.Reasons, name+": "+detail)
	}
	pass := func(name, detail string) {
		report.Checks = append(report.Checks, CollectCheck{Name: name, Status: "passed", Detail: detail})
	}

	// 1) worker-exit. A nonzero exit never passes the other checks — a
	// crashed run has no completed diff to judge — but the residue scan below
	// still runs (crashed Workers leave the most residue).
	if *rec.ExitStatus == 0 {
		pass("worker-exit", fmt.Sprintf("exit status 0 recorded by %s", rec.ExitSource))
	} else {
		fail("worker-exit", fmt.Sprintf("exit status %d recorded by %s — the Worker did not complete (docs/62 worker_failed)", *rec.ExitStatus, rec.ExitSource))
	}

	// 2) residue: session survivors are a hard reject; listeners that
	// appeared during the run but escaped the session are surfaced.
	residue, warnings, err := scanResidue(rec)
	if err != nil {
		return report, fmt.Errorf("scanning for Worker residue: %w", err)
	}
	report.Residue = residue
	report.ListenerWarnings = warnings
	if len(residue) > 0 {
		parts := make([]string, 0, len(residue))
		for _, p := range residue {
			parts = append(parts, fmt.Sprintf("pid %d %s", p.PID, p.Cmdline))
		}
		fail("residue", fmt.Sprintf("%d process(es) the Worker started are still running: %s — stop them and re-collect", len(residue), strings.Join(parts, "; ")))
	} else if rec.SessionLeaderPID <= 0 && rec.RunID == "" {
		pass("residue", "no attribution anchors recorded (pre-T0011 registry) — residue scan skipped")
	} else {
		pass("residue", fmt.Sprintf("no live processes in the Worker's session (leader pid %d) and no run-marker survivors (%s)", rec.SessionLeaderPID, rec.RunID))
	}

	if *rec.ExitStatus != 0 {
		// crashed run: nothing further to judge
		report.Status = "failed"
		return report, collectFinalize(opts, report, rec, StateWorkerFailed, fmt.Sprintf("worker exited %d", *rec.ExitStatus))
	}

	// 3) HEAD must equal the recorded baseline (a Worker whose guard was
	// bypassed moved it with a commit/checkout).
	head, err := gitOutput(rec.Worktree, "rev-parse", "HEAD")
	if err != nil {
		return report, fmt.Errorf("reading worktree HEAD: %w", err)
	}
	if head != rec.BaselineSHA {
		fail("head-baseline", fmt.Sprintf("worktree HEAD is %s, baseline was %s — the Worker moved HEAD (Git control-plane)", head, rec.BaselineSHA))
	} else {
		pass("head-baseline", fmt.Sprintf("worktree HEAD == baseline %s", rec.BaselineSHA))
	}

	// 4) the task branch ref must still point at the baseline: a commit on
	// the task branch would move it even though refsSnapshot excludes
	// refs/heads/task/** (L1-20260912-16).
	branchSHA, branchErr := gitOutput(repoRoot, "rev-parse", "--verify", "refs/heads/"+rec.Branch)
	if branchErr != nil {
		fail("branch-ref", fmt.Sprintf("reading refs/heads/%s: %v", rec.Branch, branchErr))
	} else if branchSHA != rec.BaselineSHA {
		fail("branch-ref", fmt.Sprintf("refs/heads/%s is %s, baseline was %s — the task branch moved", rec.Branch, branchSHA, rec.BaselineSHA))
	} else {
		pass("branch-ref", fmt.Sprintf("refs/heads/%s == baseline %s", rec.Branch, rec.BaselineSHA))
	}

	// 5) no unauthorized refs created (moving an existing ref is normal
	// Supervisor activity — other PRs merge while a Worker runs; creating one
	// is a Worker capability the guard denies).
	currentRefs, err := refsSnapshot(repoRoot)
	if err != nil {
		return report, err
	}
	before := map[string]bool{}
	for _, r := range rec.RefsBefore {
		before[r] = true
	}
	var newRefs []string
	for _, r := range currentRefs {
		if !before[r] {
			newRefs = append(newRefs, r)
		}
	}
	sort.Strings(newRefs)
	if len(newRefs) > 0 {
		fail("refs", fmt.Sprintf("new ref(s) created during the run: %s — creating refs is Git control-plane", strings.Join(newRefs, ", ")))
	} else {
		pass("refs", fmt.Sprintf("no new refs created (snapshot of %d refs unchanged)", len(currentRefs)))
	}

	// 6) every changed path matches allowed_scope.
	scopes, err := allowedScopeFromPackage(rec.ResultDir)
	if err != nil {
		return report, err
	}
	changed, err := changedPaths(rec.Worktree, rec.BaselineSHA)
	if err != nil {
		return report, err
	}
	report.FilesChanged = changed
	var outOfScope []string
	for _, p := range changed {
		if !pathMatchesScope(p, scopes) {
			outOfScope = append(outOfScope, p)
		}
	}
	if len(outOfScope) > 0 {
		fail("scope", fmt.Sprintf("%d changed path(s) outside allowed_scope %v: %s", len(outOfScope), scopes, strings.Join(outOfScope, ", ")))
	} else {
		pass("scope", fmt.Sprintf("%d changed path(s), all inside allowed_scope", len(changed)))
	}

	// 7) RESULT.json validates against worker-result.schema.json — the
	// mechanical enforcement the prose-only contract lacked (T0010 delivered
	// tests[].output and acceptance[] strings and was rejected).
	resultPath := filepath.Join(rec.ResultDir, "RESULT.json")
	schemaPath := filepath.Join(repoRoot, "specs", "orchestrator", "worker-result.schema.json")
	if err := ValidateWorkerResultFile(schemaPath, resultPath); err != nil {
		fail("result-schema", err.Error())
	} else {
		pass("result-schema", fmt.Sprintf("RESULT.json validates against %s", filepath.Base(schemaPath)))
	}
	if data, err := os.ReadFile(resultPath); err == nil {
		var doc struct {
			TaskID string `json:"task_id"`
		}
		if json.Unmarshal(data, &doc) == nil && doc.TaskID != taskID {
			fail("result-task-id", fmt.Sprintf("RESULT.json task_id is %q, want %q — the result belongs to a different task", doc.TaskID, taskID))
		}
	} else if len(report.Reasons) == 0 {
		fail("result-task-id", fmt.Sprintf("reading RESULT.json for the task_id check: %v", err))
	}

	// 8) no secret material introduced by the diff (or quoted in RESULT.json
	// evidence).
	secrets, err := scanSecrets(report.FilesChanged, rec.Worktree, resultPath)
	if err != nil {
		return report, err
	}
	if len(secrets) > 0 {
		fail("secrets", "secret material found: "+strings.Join(secrets, "; "))
	} else {
		pass("secrets", "no secret material found in the diff or RESULT.json")
	}

	if len(report.Reasons) > 0 {
		report.Status = "rejected"
		return report, collectFinalize(opts, report, rec, StateRejected, "collect rejected: "+strings.Join(report.Reasons, "; "))
	}
	report.Status = "ok"
	return report, collectFinalize(opts, report, rec, StateVerification, "collect: all checks passed")
}

// collectFinalize writes collect-report.json into the task's result dir and
// advances the state machine. The report write happens first so the evidence
// survives even when the transition is refused (e.g. a second collect on an
// already-verified task).
func collectFinalize(opts *CollectOpts, report *CollectReport, rec *WorkerRecord, target State, reason string) error {
	report.StateTransition = string(target)
	if err := writeFileAtomic(filepath.Join(rec.ResultDir, "collect-report.json"), marshalIndentBytes(report)); err != nil {
		return fmt.Errorf("writing collect report: %w", err)
	}
	store, err := OpenStore(opts.DagPath, opts.StatePath)
	if err != nil {
		return err
	}
	runID := opts.RunID
	if runID == "" {
		runID = NewRunID()
	}
	if _, err := store.Transition(opts.TaskID, target, runID, reason); err != nil {
		return fmt.Errorf("collection checks done (%s) but the %s -> %s transition failed: %w", report.Status, StateRunning, target, err)
	}
	return nil
}

// scanResidue returns the session- or run-marker-attributable survivors and
// the surfaced listener findings for one finished run. A legacy record
// without the spawn anchors (pre-T0011) yields no findings and no error —
// those runs predate the residue contract. Two attribution signals: the
// reaper session (every process spawn starts inherits it) and the per-run
// environment marker (POST_WORKER_RUN_ID, which real claude's Bash tool
// escapes from the session but carries into its children — T0011 Defect 2).
func scanResidue(rec *WorkerRecord) ([]ProcessFinding, []ListenerFinding, error) {
	residue := []ProcessFinding{}
	// the reaper wrapper holds the marker too (spawn sets the whole env on
	// it); its brief exit race after writing exit.status must not read as
	// residue — it is excluded like the Worker pid itself.
	exclude := map[int]bool{rec.PID: true, rec.SessionLeaderPID: true}
	if rec.SessionLeaderPID > 0 {
		found, err := sessionResidue(rec.SessionLeaderPID, rec.PID)
		if err != nil {
			return nil, nil, err
		}
		residue = append(residue, found...)
		for _, f := range found {
			exclude[f.PID] = true
		}
	}
	if marked, err := markerResidue(rec.RunID, rec.StartTime, exclude); err != nil {
		return nil, nil, err
	} else {
		residue = append(residue, marked...)
		for _, f := range marked {
			exclude[f.PID] = true
		}
	}
	sort.Slice(residue, func(i, j int) bool { return residue[i].PID < residue[j].PID })

	warnings := []ListenerFinding{}
	if len(rec.ListenersBefore) == 0 {
		return residue, warnings, nil // no baseline recorded: cannot diff
	}
	before := map[string]bool{}
	for _, a := range rec.ListenersBefore {
		before[a] = true
	}
	current, err := currentListeners()
	if err != nil {
		return nil, nil, err
	}
	for addr, li := range current {
		if before[addr] {
			continue
		}
		f := ListenerFinding{Addr: addr, Attribution: "no-owner-found"}
		owner, ok := socketOwner(li.Inode)
		if ok {
			f.PID = owner
			f.Cmdline = procCmdline(owner)
			if exclude[owner] {
				// already reported as attributable residue; the residue list
				// is the hard signal, do not double-report it as a warning
				continue
			}
			ticks, err := procStartTicks(owner)
			if err == nil && ticks > rec.StartTime {
				f.Attribution = "started-during-run"
			} else {
				f.Attribution = "pre-existing-owner"
			}
		}
		warnings = append(warnings, f)
	}
	sort.Slice(warnings, func(i, j int) bool { return warnings[i].Addr < warnings[j].Addr })
	return residue, warnings, nil
}

// changedPaths returns the repo-relative paths the Worker changed: tracked
// changes measured against the baseline (not HEAD — a moved HEAD must not
// hide the diff) plus untracked files.
func changedPaths(worktree, baseline string) ([]string, error) {
	tracked, err := gitOutput(worktree, "diff", baseline, "--name-only")
	if err != nil {
		return nil, fmt.Errorf("diffing the worktree against the baseline: %w", err)
	}
	untracked, err := gitOutput(worktree, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, fmt.Errorf("listing untracked files: %w", err)
	}
	seen := map[string]bool{}
	out := []string{}
	for _, p := range strings.Split(tracked, "\n") {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range strings.Split(untracked, "\n") {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

// pathMatchesScope reports whether p is inside any allowed_scope entry. An
// entry ending in /** matches the directory itself and everything under it;
// a plain entry matches itself or its subtree. Prefix matching is
// delimiter-safe: "cmd/rddevx" never matches "cmd/rddev/**".
func pathMatchesScope(p string, scopes []string) bool {
	for _, s := range scopes {
		if strings.HasSuffix(s, "/**") {
			base := strings.TrimSuffix(s, "/**")
			if p == base || strings.HasPrefix(p, base+"/") {
				return true
			}
			continue
		}
		base := strings.TrimSuffix(s, "/")
		if p == base || strings.HasPrefix(p, base+"/") {
			return true
		}
	}
	return false
}

// allowedScopeFromPackage reads the task package spawn validated and wrote
// into the task dir — the same allowed_scope the Worker was contracted with.
func allowedScopeFromPackage(taskDir string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(taskDir, "task-package.json"))
	if err != nil {
		return nil, fmt.Errorf("reading the task package for allowed_scope: %w", err)
	}
	var pkg TaskPackage
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, fmt.Errorf("parsing the task package: %w", err)
	}
	if len(pkg.AllowedScope) == 0 {
		return nil, fmt.Errorf("task package for %s declares no allowed_scope", pkg.TaskID)
	}
	return pkg.AllowedScope, nil
}

// scanSecrets scans the changed files' current content (deleted files carry
// no content) plus RESULT.json for the documented credential shapes. Binary
// files are skipped. Findings name file:line and the pattern class only.
func scanSecrets(files []string, worktree, resultPath string) ([]string, error) {
	targets := make([]string, 0, len(files)+1)
	for _, f := range files {
		targets = append(targets, filepath.Join(worktree, f))
	}
	targets = append(targets, resultPath)
	findings := []string{}
	for _, path := range targets {
		data, err := os.ReadFile(path)
		if err != nil {
			continue // deleted or moved file — nothing to scan
		}
		if len(data) > 8192 && strings.ContainsRune(string(data[:8192]), 0) {
			continue // binary
		}
		for i, ln := range strings.Split(string(data), "\n") {
			for _, p := range secretPatterns {
				if p.re.MatchString(ln) {
					findings = append(findings, fmt.Sprintf("%s:%d contains a %s", path, i+1, p.name))
				}
			}
		}
	}
	sort.Strings(findings)
	return findings, nil
}
