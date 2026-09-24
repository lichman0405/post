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
	// PGID is the process group — the unit a signal must target to reach the
	// survivors and not just the process named here (#166). 0 when unreadable.
	PGID int `json:"pgid"`
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
	// disk-derived path `worker list` uses; the authoritative copy in the
	// Supervisor-owned runtime dir wins, T0012), then re-read.
	if _, err := DiscoverWorkers(repoRoot); err != nil {
		return nil, err
	}
	rec, err = LoadRegistry(repoRoot, taskID)
	if err != nil {
		return nil, err
	}
	if rec.ExitStatus == nil {
		return nil, fmt.Errorf("worker %s has no recorded exit status (still running or stale) — collect refuses to judge an unfinished run; see `rddev worker list`", taskID)
	}

	// The authoritative spawn-time record (T0012 security fix). Collect
	// judges by THESE values — baseline, allowed_scope, package and guard
	// bytes — never by the Worker-writable copies in the task dir. A missing
	// record means a pre-T0012 spawn: the judgement degrades to the registry
	// values with a loud warning, never a silent pass.
	gate, gateErr := LoadGateInputs(repoRoot, taskID)
	if gateErr != nil {
		return nil, fmt.Errorf("loading authoritative gate inputs for %s: %w", taskID, gateErr)
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

	// 0) gate-input integrity: the Worker-writable copies of the gate inputs
	// (registry gate fields, task-package.json, guard layer, exit.status)
	// must byte-match the spawn-time authoritative record. A mismatch is a
	// hard reject — the Worker edited its own gate inputs.
	if gate != nil {
		if tampers := VerifyGateInputs(repoRoot, rec, gate); len(tampers) > 0 {
			parts := make([]string, 0, len(tampers))
			for _, t := range tampers {
				parts = append(parts, t.What+": "+t.Detail)
			}
			fail("gate-inputs", fmt.Sprintf("%d tamper(s) detected against the authoritative spawn record: %s", len(tampers), strings.Join(parts, "; ")))
		} else {
			pass("gate-inputs", "Worker-writable gate inputs byte-match the authoritative spawn record (registry fields, task-package.json, guard layer, exit.status)")
		}
	} else {
		pass("gate-inputs", "no authoritative spawn record (pre-T0012 spawn) — judgement degrades to the registry values with this warning")
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
		fail("residue", residueReport("Worker", rec, residue))
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
	// bypassed moved it with a commit/checkout). The baseline is the
	// AUTHORITATIVE spawn record's value (T0012); the registry copy is only
	// the fallback for pre-T0012 spawns.
	baseline := rec.BaselineSHA
	if gate != nil {
		baseline = gate.BaselineSHA
	}
	head, err := gitOutput(rec.Worktree, "rev-parse", "HEAD")
	if err != nil {
		return report, fmt.Errorf("reading worktree HEAD: %w", err)
	}
	if head != baseline {
		fail("head-baseline", fmt.Sprintf("worktree HEAD is %s, baseline was %s — the Worker moved HEAD (Git control-plane)", head, baseline))
	} else {
		pass("head-baseline", fmt.Sprintf("worktree HEAD == baseline %s", baseline))
	}

	// 4) the task branch ref must still point at the baseline: a commit on
	// the task branch would move it even though refsSnapshot excludes
	// refs/heads/task/** (L1-20260912-16).
	branchSHA, branchErr := gitOutput(repoRoot, "rev-parse", "--verify", "refs/heads/"+rec.Branch)
	if branchErr != nil {
		fail("branch-ref", fmt.Sprintf("reading refs/heads/%s: %v", rec.Branch, branchErr))
	} else if branchSHA != baseline {
		fail("branch-ref", fmt.Sprintf("refs/heads/%s is %s, baseline was %s — the task branch moved", rec.Branch, branchSHA, baseline))
	} else {
		pass("branch-ref", fmt.Sprintf("refs/heads/%s == baseline %s", rec.Branch, baseline))
	}

	// 5) no unauthorized refs created (moving an existing ref is normal
	// Supervisor activity — other PRs merge while a Worker runs; creating one
	// is a Worker capability the guard denies).
	currentRefs, err := refsSnapshot(repoRoot)
	if err != nil {
		return report, err
	}
	// Whose ref is it? By RECORD, not by inference.
	//
	// The question that separates the two cases is not the ref's name — a Worker
	// turning its work into a ref and the Supervisor opening a pull-request
	// branch are both "a new ref" — it is whether the Supervisor created it. The
	// Supervisor writes down every ref it creates (spawn, commit, rebaseline,
	// reconciliation, and `rddev refs adopt` for anything opened by hand); a new
	// ref name that is not on that record is unattributable and is a finding.
	//
	// The rule before this one read the commit's author email. That field is set
	// by whoever makes the commit (`git -c user.email=…`), so it was fail-open
	// against exactly the Worker that got past the guard — see ref_ledger.go for
	// the three rules this replaces and why each was wrong.
	ledger, err := ReadSupervisorRefs(repoRoot)
	if err != nil {
		// Fail closed, and loudly: without the ledger "the Supervisor created
		// this ref" is unknowable, and answering it either way would be worse
		// than stopping the collect.
		return report, err
	}
	newRefs := unattributableNewRefs(rec.RefsBefore, currentRefs, ledger)
	if len(newRefs) > 0 {
		fail("refs", fmt.Sprintf("new ref(s) created during the run: %s — creating refs is Git control-plane. If a ref here is the Supervisor's own work, record it (`rddev refs adopt <name>`) rather than widening the check", strings.Join(newRefs, ", ")))
	} else {
		pass("refs", fmt.Sprintf("no unattributable new ref names (snapshot of %d refs, existing ones free to move; %d Supervisor ref(s) on record)", len(currentRefs), len(ledger)))
	}

	// 6) every changed path matches allowed_scope. The scope comes from the
	// AUTHORITATIVE spawn record (T0012 — the Worker-writable task-package
	// copy is compared against it above, never used as the judgement input),
	// plus the derived-artifact allowance: a task whose scope covers a
	// derived marker may regenerate the artifact derived from it (e.g.
	// specs/** -> specs/SPEC_VERSION.json).
	var scopes []string
	if gate != nil {
		scopes = gate.AllowedScope
	} else {
		scopes, err = allowedScopeFromPackage(rec.ResultDir)
		if err != nil {
			return report, err
		}
	}
	changed, err := changedPaths(rec.Worktree, baseline)
	if err != nil {
		return report, err
	}
	report.FilesChanged = changed
	derived := derivedAt(repoRoot)
	var outOfScope []string
	for _, p := range changed {
		if !ScopeMatchesPathWithDerived(p, scopes, derived) {
			outOfScope = append(outOfScope, p)
		}
	}
	if len(outOfScope) > 0 {
		fail("scope", fmt.Sprintf("%d changed path(s) outside allowed_scope %v: %s", len(outOfScope), scopes, strings.Join(outOfScope, ", ")))
	} else {
		pass("scope", fmt.Sprintf("%d changed path(s), all inside allowed_scope (derived-artifact allowance applied)", len(changed)))
	}

	// 7) RESULT.json belongs to THIS attempt (#264), then validates against
	// worker-result.schema.json — the mechanical enforcement the prose-only
	// contract lacked (T0010 delivered tests[].output and acceptance[] strings
	// and was rejected).
	//
	// Freshness first, because it is the question the schema cannot ask. A
	// re-dispatch reuses the result dir and the file name (rework resumes the
	// session and keeps the worktree; respawn resets the worktree only), so a
	// rework whose Worker never rewrote its RESULT.json handed the previous
	// attempt's document to the judge — schema-valid, task_id correct, status
	// completed, and green. Whose document this is has to be judged against
	// something the Worker does not write; see result_freshness.go for the two
	// instruments and what each is blind to.
	resultPath := filepath.Join(rec.ResultDir, "RESULT.json")
	freshness, err := CheckResultFreshness(resultPath, gate, rec)
	if err != nil {
		return report, err
	}
	for _, f := range freshness {
		if f.Refused {
			fail("result-freshness-"+string(f.Instrument), f.Detail)
		} else {
			pass("result-freshness-"+string(f.Instrument), f.Detail)
		}
	}
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

	// 7b) RESULT consistency (T0012 requirement 1): a completion claim is
	// verified against the document itself. An INTERIM-marked file is refused
	// outright; `status: completed` with any not_run/failed test or a
	// non-passed acceptance entry is a contradiction and is refused
	// mechanically — the defect where an INTERIM snapshot with two not_run
	// tests was collected as a completion can never pass again. A
	// failed/blocked RESULT is an honest non-completion: it is rejected below,
	// never accepted.
	criteria := []string(nil)
	requiredTests := []string(nil)
	if gate != nil {
		criteria = gate.AcceptanceCriteria
		requiredTests = gate.RequiredTests
	}
	consistencyChecks, _, consErr := CheckResultConsistency(resultPath, criteria, requiredTests)
	if consErr != nil {
		return report, consErr
	}
	for _, c := range consistencyChecks {
		if c.Status == "failed" {
			fail("result-consistency", c.Name+": "+c.Detail)
		} else {
			pass("result-consistency-"+c.Name, c.Detail)
		}
	}
	// An honest non-completion (status failed/blocked) is a rejected run,
	// never verification — checked UNCONDITIONALLY: CheckResultConsistency
	// reports consistent=false for it without a failed check, so nesting this
	// under `consistent` made the refusal dead code (the rework e2e caught a
	// failed RESULT reaching verification).
	if raw, err := os.ReadFile(resultPath); err == nil {
		var doc WorkerResultDoc
		if json.Unmarshal(raw, &doc) == nil && doc.Status != "completed" {
			fail("result-status", fmt.Sprintf("RESULT.json reports status %q — an honest non-completion is a rejected run, never verification", doc.Status))
		}
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
// already-verified task). The gate evidence record (T0012) is written into
// the Supervisor-owned gates dir either way — the collect outcome is part of
// the four-gate trail, whatever the verdict.
func collectFinalize(opts *CollectOpts, report *CollectReport, rec *WorkerRecord, target State, reason string) error {
	report.StateTransition = string(target)
	reportPath := filepath.Join(rec.ResultDir, "collect-report.json")
	if err := writeFileAtomic(reportPath, marshalIndentBytes(report)); err != nil {
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
	if _, err := WriteRecord(opts.RepoRoot, opts.TaskID, RecordCollect, runID, &CollectRecord{
		recordMeta:  recordMeta{RecordType: RecordCollect, TaskID: opts.TaskID, RunID: runID, At: nowRFC3339()},
		Status:      report.Status,
		ReportPath:  reportPath,
		ResultState: string(target),
		Summary:     reason,
	}); err != nil {
		return fmt.Errorf("collect transition done but the gate evidence record failed to write: %w", err)
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

// unattributableNewRefs returns the refs that appeared during the run and are
// NOT on the Supervisor's own record of refs it created (ref_ledger.go). This
// is the collected rule, factored out so the test asserts the same function
// collect executes rather than a paraphrase of it.
func unattributableNewRefs(before, current []string, ledger map[string]SupervisorRef) []string {
	var out []string
	for _, r := range newRefsSince(before, current) {
		if _, ok := ledger[refName(r)]; ok {
			continue
		}
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// refName returns the ref name from a snapshot entry of the form
// "<refname> <objectname>". An entry without a sha (older records, fixtures)
// is returned unchanged.
func refName(entry string) string {
	if i := strings.IndexByte(entry, ' '); i >= 0 {
		return entry[:i]
	}
	return entry
}

// newRefsSince returns the entries of current whose ref NAME was absent from
// before.
//
// Names, not whole entries. refsSnapshot stores "<refname> <objectname>", so
// comparing entries made any ref that merely MOVED look newly created — and
// refs move routinely while a Worker runs, because that is the Supervisor
// merging other people's work. The policy this implements is the one the
// surrounding comment always stated: creating a ref is a Worker capability the
// guard denies, moving one is normal Supervisor activity. T0101 was rejected
// with "new ref(s) created: refs/heads/main a07ac01…" purely because two
// unrelated PRs merged during its run.
func newRefsSince(before, current []string) []string {
	seen := make(map[string]bool, len(before))
	for _, r := range before {
		seen[refName(r)] = true
	}
	var out []string
	for _, r := range current {
		if !seen[refName(r)] {
			out = append(out, r)
		}
	}
	return out
}
