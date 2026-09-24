package devorchestrator

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The #264 defect, tested as a behaviour: what `rddev worker collect` decides
// about a RESULT.json that an earlier round left behind.
//
// The fixture constructs the state the defect needs — a task dir holding the
// previous attempt's document, then a re-dispatch that reuses it — instead of
// racing a real rework, because the state, not the timing, is what the
// judgement is about. Everything else is the real pipeline: a real git repo
// with a real worktree at the baseline, the real authoritative gate-inputs
// record written by WriteGateInputs, the real registry, and the real
// Collect().

// freshnessRepo is a repository Collect can judge: baseline commit, task
// branch + worktree, DAG, running task state, the real RESULT schema, and the
// task's runtime dirs.
type freshnessRepo struct {
	repo      string
	taskID    string
	worktree  string
	taskDir   string
	baseline  string
	dagPath   string
	statePath string
	refs      []string
}

func newFreshnessRepo(t *testing.T, taskID string) *freshnessRepo {
	t.Helper()
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", DefaultBaseBranch)

	// The repo's OWN schema, copied: collect validates the RESULT against the
	// repository's copy, so a fixture with a stub schema would be testing a
	// contract nobody has.
	schema, err := os.ReadFile(filepath.Join("..", "..", "specs", "orchestrator", "worker-result.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "specs", "orchestrator"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "specs", "orchestrator", "worker-result.schema.json"), schema, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	baseline := git("rev-parse", "HEAD")
	git("branch", "task/"+taskID+"-x")

	// The spawn-time ref snapshot, taken once the task branch exists and
	// before the worktree is materialised — the same order spawn uses, so the
	// fixture's run has no new refs at collect.
	refs, err := refsSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(repo, ".rddev", "worktrees", taskID)
	git("worktree", "add", "-q", worktree, "task/"+taskID+"-x")

	tasksDir := filepath.Join(repo, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dagPath := writeDAG(t, tasksDir)
	statePath := filepath.Join(tasksDir, "task_status.json")
	if err := os.WriteFile(statePath, []byte(`{"version":1,"tasks":{"`+taskID+`":{"status":"running"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	taskDir := WorkerTaskDir(repo, taskID)
	if err := os.MkdirAll(filepath.Join(taskDir, "guard"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The guard layer WriteGateInputs byte-copies at dispatch.
	for name, body := range map[string]string{
		"task-package.json":     `{"task_id":"` + taskID + `","allowed_scope":["internal/devorchestrator/**"]}`,
		"guard/worker-guard.sh": "#!/bin/sh\nexit 0\n",
		"worker-settings.json":  "{}\n",
		"run-worker.sh":         "#!/bin/sh\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(taskDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &freshnessRepo{
		repo: repo, taskID: taskID, worktree: worktree, taskDir: taskDir,
		baseline: baseline, dagPath: dagPath, statePath: statePath, refs: refs,
	}
}

// dispatch records one attempt exactly the way spawn does: the authoritative
// gate inputs first (which stamp DispatchedAt and the digest of whatever
// RESULT.json is already there), then the process identity, then the
// registry.
func (f *freshnessRepo) dispatch(t *testing.T, runID string) (*WorkerRecord, *GateInputs) {
	t.Helper()
	gate := &GateInputs{
		TaskID: f.taskID, RunID: runID, SessionID: "s-" + runID,
		BaselineSHA: f.baseline, Branch: "task/" + f.taskID + "-x",
		Worktree: f.worktree, ResultDir: f.taskDir,
		LogPath:    filepath.Join(f.taskDir, "worker-"+runID+".log"),
		RefsBefore: f.refs, AllowedScope: []string{"internal/devorchestrator/**"},
		RequiredTests: []string{}, AcceptanceCriteria: []string{},
	}
	// The pre-process write: this is what stamps DispatchedAt and
	// ResultSHAAtSpawn, and it must run before this attempt writes anything.
	if err := WriteGateInputs(f.repo, f.taskID, gate, f.taskDir); err != nil {
		t.Fatal(err)
	}
	// ... and the post-fork finalize, so StartedAt is LATER than DispatchedAt
	// exactly as it is in a real run (spawn reads /proc and `claude --version`
	// first). A fixture that set them the other way round would never exercise
	// the precedence the judgement rests on.
	pid, startTime := exitedProcess(t)
	startedAt := runStartedAt()
	if err := FinalizeGateInputs(f.repo, f.taskID, gate.RunID, gate.SessionID, pid, startTime, 0, startedAt, nil); err != nil {
		t.Fatal(err)
	}
	rec := &WorkerRecord{
		TaskID: f.taskID, RunID: gate.RunID, SessionID: gate.SessionID,
		ClaudeVersion: "fixture", PID: pid, StartTime: startTime,
		Worktree: f.worktree, Branch: gate.Branch, BaselineSHA: f.baseline,
		RefsBefore: f.refs, LogPath: gate.LogPath, ResultDir: f.taskDir,
		StartedAt: startedAt,
	}
	exited := 0
	rec.ExitStatus = &exited
	rec.ExitSource = ExitSourceReaper
	if err := SaveRegistry(f.repo, rec); err != nil {
		t.Fatal(err)
	}
	return rec, gate
}

// resultPath is the file the freshness judgement reads.
func (f *freshnessRepo) resultPath() string { return filepath.Join(f.taskDir, "RESULT.json") }

// writeDocument writes the attempt's deliverable and a RESULT.json naming it.
// The bytes depend only on summary, so writing the same summary twice
// reproduces the same document — which is how the mtime-refresh case is built.
func (f *freshnessRepo) writeDocument(t *testing.T, summary string) {
	t.Helper()
	deliverable := filepath.Join(f.worktree, "internal", "devorchestrator", "deliverable.txt")
	if err := os.MkdirAll(filepath.Dir(deliverable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deliverable, []byte(summary+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{
		"task_id": f.taskID, "status": "completed", "summary": summary,
		"files_changed":        []string{"internal/devorchestrator/deliverable.txt"},
		"tests":                []map[string]string{{"command": "fixture", "status": "passed", "evidence": "fixture"}},
		"acceptance":           []map[string]string{},
		"risks":                []string{},
		"follow_up_issues":     []string{},
		"notes_for_supervisor": "fixture",
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.resultPath(), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// age backdates a file, which is what "an earlier round wrote this" means on
// disk. The judgement is about where a document sits in time, so the test has
// to be able to place it.
func age(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func (f *freshnessRepo) collect(t *testing.T) *CollectReport {
	t.Helper()
	report, err := Collect(&CollectOpts{RepoRoot: f.repo, DagPath: f.dagPath, StatePath: f.statePath, TaskID: f.taskID, RunID: "collect-run"})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	return report
}

func (f *freshnessRepo) taskState(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.statePath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Tasks map[string]struct {
			Status string `json:"status"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Tasks[f.taskID].Status
}

// freshnessChecks lists the freshness checks that FAILED, with their details.
func freshnessChecks(report *CollectReport) map[string]string {
	out := map[string]string{}
	for _, c := range report.Checks {
		if strings.HasPrefix(c.Name, "result-freshness-") && c.Status == "failed" {
			out[c.Name] = c.Detail
		}
	}
	return out
}

// freshnessPasses lists the freshness checks that ran and said yes. Asserted
// on the accepting cases as well as the refusing ones: a verdict of "ok" is
// also what a report with no freshness check at all looks like, so the
// accepting cases have to show the question was asked.
func freshnessPasses(report *CollectReport) []string {
	out := []string{}
	for _, c := range report.Checks {
		if strings.HasPrefix(c.Name, "result-freshness-") && c.Status == "passed" {
			out = append(out, c.Name)
		}
	}
	return out
}

// TestAStaleResultJsonFromAnEarlierRoundIsNotCollectedAsThisRounds is the
// defect itself (#264): a rework that never rewrote its RESULT.json used to
// have the previous attempt's document collected as its own — schema-valid,
// task_id right, status completed, verdict green. Every subtest drives the
// real Collect() and asserts the OUTCOME (the verdict in the report and the
// state the task lands in), not the shape of any function.
func TestAStaleResultJsonFromAnEarlierRoundIsNotCollectedAsThisRounds(t *testing.T) {
	hourAgo := time.Now().Add(-time.Hour)

	t.Run("the earlier round's document, untouched", func(t *testing.T) {
		f := newFreshnessRepo(t, "T0001")
		// Attempt 1 wrote this and was rejected for it.
		f.writeDocument(t, "attempt 1's completion claim")
		age(t, f.resultPath(), hourAgo)
		// Attempt 2 is dispatched, and its Worker writes nothing at all.
		f.dispatch(t, "run-2")

		report := f.collect(t)
		if report.Status != "rejected" {
			t.Fatalf("collect verdict = %q, want rejected — the earlier round's document was collected as this round's. checks: %+v", report.Status, report.Checks)
		}
		failed := freshnessChecks(report)
		if _, ok := failed["result-freshness-dispatch-content"]; !ok {
			t.Errorf("the byte-identity check did not fire on the very document dispatch recorded: %v", failed)
		}
		if _, ok := failed["result-freshness-dispatch-time"]; !ok {
			t.Errorf("the dispatch-time check did not fire on an hour-old document: %v", failed)
		}
		if got := f.taskState(t); got != string(StateRejected) {
			t.Errorf("task state = %s, want rejected — a stale document must never carry a task into verification", got)
		}
	})

	t.Run("the same document with its mtime refreshed", func(t *testing.T) {
		f := newFreshnessRepo(t, "T0001")
		f.writeDocument(t, "attempt 1's completion claim")
		age(t, f.resultPath(), hourAgo)
		f.dispatch(t, "run-2")
		// Something re-materialised the file — a restore, a copy, a checkout —
		// so its mtime says "now" while its bytes are the earlier round's.
		// This is the shape an mtime-only criterion accepts.
		age(t, f.resultPath(), time.Now())

		report := f.collect(t)
		if report.Status != "rejected" {
			t.Fatalf("collect verdict = %q, want rejected — a refreshed mtime must not launder an earlier round's document. checks: %+v", report.Status, report.Checks)
		}
		failed := freshnessChecks(report)
		if _, ok := failed["result-freshness-dispatch-content"]; !ok {
			t.Errorf("the mtime-refreshed document was not refused by the byte-identity check: %v", failed)
		}
		if _, ok := failed["result-freshness-dispatch-time"]; ok {
			t.Errorf("the dispatch-time check fired on a document whose mtime is now — the case is not constructed as intended: %v", failed)
		}
	})

	t.Run("a document this attempt wrote", func(t *testing.T) {
		f := newFreshnessRepo(t, "T0001")
		f.writeDocument(t, "attempt 1's completion claim")
		age(t, f.resultPath(), hourAgo)
		f.dispatch(t, "run-2")
		f.writeDocument(t, "attempt 2 delivered")

		report := f.collect(t)
		if report.Status != "ok" {
			t.Fatalf("collect verdict = %q, want ok — rewrite the document and collect must accept it. reasons: %v, checks: %+v", report.Status, report.Reasons, report.Checks)
		}
		if got := freshnessPasses(report); len(got) != 2 {
			t.Errorf("freshness checks that ran = %v, want both instruments asked and answered", got)
		}
		if got := f.taskState(t); got != string(StateVerification) {
			t.Errorf("task state = %s, want verification", got)
		}
	})

	t.Run("a document this attempt wrote and then rewrote", func(t *testing.T) {
		// The contract's own shape: "write it EARLY and overwrite it as
		// evidence improves". An interim write followed by the final one is
		// inside the attempt and must not be refused as stale.
		f := newFreshnessRepo(t, "T0001")
		f.dispatch(t, "run-1")
		f.writeDocument(t, "interim")
		f.writeDocument(t, "final")

		report := f.collect(t)
		if report.Status != "ok" {
			t.Fatalf("collect verdict = %q, want ok — a rewrite inside the attempt is not staleness. reasons: %v", report.Status, report.Reasons)
		}
	})

	t.Run("a first dispatch with no earlier document", func(t *testing.T) {
		// The content instrument has nothing to fingerprint here, so the
		// judgement rests on the dispatch instant alone: the case that must
		// stay green, and the one an over-eager fingerprint would break.
		f := newFreshnessRepo(t, "T0002")
		f.dispatch(t, "run-1")
		f.writeDocument(t, "delivered")

		report := f.collect(t)
		if report.Status != "ok" {
			t.Fatalf("collect verdict = %q, want ok on a first dispatch. reasons: %v", report.Status, report.Reasons)
		}
		// Inert, not missing: with no earlier document there is nothing to
		// fingerprint, and the time instrument carries the judgement alone.
		if got := freshnessPasses(report); len(got) != 1 || got[0] != "result-freshness-dispatch-time" {
			t.Errorf("freshness checks that ran = %v, want the dispatch-time instrument only", got)
		}
	})

	t.Run("the same bytes written again by this attempt", func(t *testing.T) {
		// The one refusal that is a false hit, pinned here rather than left to
		// be discovered: a Worker that re-delivers the previous document
		// byte-for-byte is indistinguishable, from the document alone, from one
		// that wrote nothing at all — and by this contract's own words
		// ("`completed` means 'submitted for acceptance'", "overwrite it as
		// evidence improves") those are the same report, so both are refused,
		// with a message that says what to do. The alternative is to accept a
		// document whose every claim is the previous round's.
		f := newFreshnessRepo(t, "T0001")
		f.writeDocument(t, "attempt 1's completion claim")
		age(t, f.resultPath(), hourAgo)
		f.dispatch(t, "run-2")
		f.writeDocument(t, "attempt 1's completion claim")

		report := f.collect(t)
		if report.Status != "rejected" {
			t.Fatalf("collect verdict = %q, want rejected — a byte-identical redelivery carries no work of this attempt's. checks: %+v", report.Status, report.Checks)
		}
		failed := freshnessChecks(report)
		detail, ok := failed["result-freshness-dispatch-content"]
		if !ok {
			t.Fatalf("the byte-identity check did not fire on an identical redelivery: %v", failed)
		}
		if !strings.Contains(detail, "Write the document for THIS attempt") {
			t.Errorf("the refusal does not tell the Worker what to do about it: %s", detail)
		}
	})
}

// TestTheFreshnessJudgementNamesWhatItMeasured: the instruments themselves,
// including the paths where the judgement has to fail closed. Each case states
// which question was asked and what it answered, because a refusal a Worker
// cannot act on is not a repair.
func TestTheFreshnessJudgementNamesWhatItMeasured(t *testing.T) {
	dir := t.TempDir()
	resultPath := filepath.Join(dir, "RESULT.json")
	body := []byte(`{"task_id":"T0001"}`)
	write := func(mtime time.Time) {
		t.Helper()
		if err := os.WriteFile(resultPath, body, 0o644); err != nil {
			t.Fatal(err)
		}
		age(t, resultPath, mtime)
	}
	now := time.Now()
	dispatch := runStartedAtFrom(now.Add(-time.Minute))

	t.Run("no document is nobody's business", func(t *testing.T) {
		findings, err := CheckResultFreshness(filepath.Join(dir, "absent.json"), nil, nil)
		if err != nil || len(findings) != 0 {
			t.Fatalf("findings = %v, err = %v, want none — the schema check reports a missing RESULT.json", findings, err)
		}
	})

	t.Run("an older document is refused by both instruments", func(t *testing.T) {
		write(now.Add(-time.Hour))
		gate := &GateInputs{DispatchedAt: dispatch, ResultSHAAtSpawn: sha256Hex(body)}
		findings, err := CheckResultFreshness(resultPath, gate, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(findings) != 2 {
			t.Fatalf("got %d findings, want 2 (one per instrument): %+v", len(findings), findings)
		}
		for _, f := range findings {
			if !f.Refused {
				t.Errorf("%s accepted an hour-old document: %s", f.Instrument, f.Detail)
			}
		}
	})

	t.Run("a fresh document is placed by both instruments", func(t *testing.T) {
		write(now)
		gate := &GateInputs{DispatchedAt: dispatch, ResultSHAAtSpawn: sha256Hex([]byte("something else entirely"))}
		findings, err := CheckResultFreshness(resultPath, gate, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(findings) != 2 {
			t.Fatalf("got %d findings, want 2 (one per instrument)", len(findings))
		}
		for _, f := range findings {
			if f.Refused {
				t.Errorf("%s refused a document this attempt wrote: %s", f.Instrument, f.Detail)
			}
			if !strings.Contains(f.Detail, "sha256") && !strings.Contains(f.Detail, "after this attempt was dispatched") {
				t.Errorf("%s detail does not say what it measured: %s", f.Instrument, f.Detail)
			}
		}
	})

	t.Run("no dispatch instant at all fails closed", func(t *testing.T) {
		write(now)
		findings, err := CheckResultFreshness(resultPath, nil, &WorkerRecord{TaskID: "T0001"})
		if err != nil {
			t.Fatal(err)
		}
		if len(findings) != 1 || !findings[0].Refused {
			t.Fatalf("findings = %+v, want a single refusal: with no reading at all, which attempt wrote this document is unknowable", findings)
		}
	})

	t.Run("a registry start stands in when there is no dispatch record", func(t *testing.T) {
		// Pre-T0012 degraded path: the registry's started_at is the only
		// reading, and a document older than it is still refused.
		write(now.Add(-time.Hour))
		findings, err := CheckResultFreshness(resultPath, nil, &WorkerRecord{TaskID: "T0001", StartedAt: runStartedAtFrom(now.Add(-time.Minute))})
		if err != nil {
			t.Fatal(err)
		}
		if len(findings) != 1 || !findings[0].Refused {
			t.Fatalf("findings = %+v, want a refusal from the registry reading", findings)
		}
	})

	t.Run("the dispatch record wins over the later registry start", func(t *testing.T) {
		// StartedAt is taken after the process exists, so a document written
		// in between belongs to this attempt. Judging by StartedAt alone would
		// refuse it; the dispatch reading does not. This is the window the
		// review path documents and the reason this one does not reuse its
		// comparison.
		written := now.Add(-30 * time.Second)
		write(written)
		gate := &GateInputs{DispatchedAt: runStartedAtFrom(written.Add(-time.Second))}
		rec := &WorkerRecord{TaskID: "T0001", StartedAt: runStartedAtFrom(written.Add(time.Second))}
		findings, err := CheckResultFreshness(resultPath, gate, rec)
		if err != nil {
			t.Fatal(err)
		}
		if len(findings) != 1 || findings[0].Refused {
			t.Fatalf("findings = %+v, want acceptance: the document was written after dispatch, whatever the later registry start says", findings)
		}
		if !strings.Contains(findings[0].Detail, "authoritative dispatch record") {
			t.Errorf("the judgement did not say which reading it used: %s", findings[0].Detail)
		}
	})
}
