package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lichman0405/post/internal/devorchestrator"
)

// taskEnv is a scratch DAG/state pair for CLI tests: A -> B -> C plus a
// no-deps task D (same shape as the store's synthetic DAG).
type taskEnv struct {
	dir       string
	dagPath   string
	statePath string
}

func newTaskEnv(t *testing.T) *taskEnv {
	t.Helper()
	dir := t.TempDir()
	dag := map[string]any{
		"version":    1,
		"task_count": 4,
		"tasks": []map[string]any{
			{"id": "T1000", "phase": "P1", "title": "task A", "dependencies": []string{}},
			{"id": "T1001", "phase": "P1", "title": "task B", "dependencies": []string{"T1000"}},
			{"id": "T1002", "phase": "P1", "title": "task C", "dependencies": []string{"T1001"}},
			{"id": "T1003", "phase": "P1", "title": "task D", "dependencies": []string{}},
		},
	}
	writeJSONFile(t, filepath.Join(dir, "tasks.json"), dag)
	return &taskEnv{
		dir:       dir,
		dagPath:   filepath.Join(dir, "tasks.json"),
		statePath: filepath.Join(dir, "task_status.json"),
	}
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedState writes the state file with the given statuses (fresh file).
func (e *taskEnv) seedState(t *testing.T, statuses map[string]devorchestrator.State) {
	t.Helper()
	tasks := map[string]map[string]any{}
	for id, st := range statuses {
		tasks[id] = map[string]any{"status": string(st)}
	}
	writeJSONFile(t, e.statePath, map[string]any{"version": 2, "tasks": tasks})
}

// readState returns the recorded TaskState for id (todo when absent).
func (e *taskEnv) readState(t *testing.T, id string) devorchestrator.TaskState {
	t.Helper()
	data, err := os.ReadFile(e.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return devorchestrator.TaskState{Status: devorchestrator.StateTodo}
		}
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	tasks := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw["tasks"], &tasks); err != nil {
		t.Fatal(err)
	}
	var ts devorchestrator.TaskState
	if err := json.Unmarshal(tasks[id], &ts); err != nil {
		t.Fatal(err)
	}
	return ts
}

// runTaskCLI invokes the task subcommand with the scratch paths wired in.
func (e *taskEnv) runTaskCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	full := append([]string{"--tasks-json", e.dagPath, "--state-json", e.statePath}, args...)
	code = runTask(full, &out, &errb, false)
	return code, out.String(), errb.String()
}

// nextIDs parses `rddev task next --json` into its task id list.
func nextIDs(t *testing.T, stdout string) []string {
	t.Helper()
	var doc struct {
		Next []devorchestrator.NextTask `json:"next"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("next output is not valid JSON: %v\n%s", err, stdout)
	}
	ids := make([]string, len(doc.Next))
	for i, n := range doc.Next {
		ids[i] = n.ID
	}
	return ids
}

func TestCLINextOnlyDependencySatisfied(t *testing.T) {
	e := newTaskEnv(t)
	// Drive T1000 to merged via the store (the CLI surface for the full
	// worker lifecycle belongs to T0010/T0011).
	s, err := devorchestrator.OpenStore(e.dagPath, e.statePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range []devorchestrator.State{
		devorchestrator.StateReady, devorchestrator.StateRunning,
		devorchestrator.StateVerification, devorchestrator.StateAccepted,
		devorchestrator.StateMerged,
	} {
		if _, err := s.Transition("T1000", to, devorchestrator.NewRunID(), ""); err != nil {
			t.Fatal(err)
		}
	}

	code, out, stderr := e.runTaskCLI(t, "next", "--json")
	if code != 0 {
		t.Fatalf("next exit code = %d, stderr = %q", code, stderr)
	}
	got := nextIDs(t, out)
	want := []string{"T1001", "T1003"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("next = %v, want %v (T1002 excluded: dependency not merged)", got, want)
	}

	// Human output is one line per task with id and title.
	code, out, stderr = e.runTaskCLI(t, "next")
	if code != 0 {
		t.Fatalf("next (human) exit code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"T1001", "T1003", "task B", "task D"} {
		if !strings.Contains(out, want) {
			t.Errorf("next (human) output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "T1002") {
		t.Errorf("next (human) listed T1002 with unmet dependency:\n%s", out)
	}
}

func TestCLIReadyRequiresDependenciesMerged(t *testing.T) {
	e := newTaskEnv(t)
	code, _, stderr := e.runTaskCLI(t, "ready", "T1001", "--run-id", "run-test")
	if code != 1 {
		t.Fatalf("ready with unmet dep exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "dependencies not merged") || !strings.Contains(stderr, "T1000") {
		t.Errorf("stderr = %q, want dependency error naming T1000", stderr)
	}
	if _, err := os.Stat(e.statePath); !os.IsNotExist(err) {
		t.Errorf("state file created by a rejected ready transition")
	}

	// After T1000 is merged, ready succeeds and stamps the supplied run id.
	s, _ := devorchestrator.OpenStore(e.dagPath, e.statePath)
	for _, to := range []devorchestrator.State{
		devorchestrator.StateReady, devorchestrator.StateRunning,
		devorchestrator.StateVerification, devorchestrator.StateAccepted,
		devorchestrator.StateMerged,
	} {
		if _, err := s.Transition("T1000", to, devorchestrator.NewRunID(), ""); err != nil {
			t.Fatal(err)
		}
	}
	code, out, stderr := e.runTaskCLI(t, "ready", "T1001", "--run-id", "run-test")
	if code != 0 {
		t.Fatalf("ready exit code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(out, "todo -> ready") || !strings.Contains(out, "run_id=run-test") {
		t.Errorf("ready output = %q, want transition with run_id", out)
	}
	ts := e.readState(t, "T1001")
	if ts.Status != devorchestrator.StateReady {
		t.Errorf("T1001 status = %s, want ready", ts.Status)
	}
	if len(ts.History) != 1 || ts.History[0].RunID != "run-test" {
		t.Errorf("T1001 history = %+v, want one entry with run-test", ts.History)
	}
}

func TestCLIInspectJSONShape(t *testing.T) {
	e := newTaskEnv(t)
	e.seedState(t, map[string]devorchestrator.State{"T1000": devorchestrator.StateAccepted})
	code, out, stderr := e.runTaskCLI(t, "inspect", "T1000", "--json")
	if code != 0 {
		t.Fatalf("inspect exit code = %d, stderr = %q", code, stderr)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("inspect output is not valid JSON: %v", err)
	}
	task, state := doc["task"].(map[string]any), doc["state"].(map[string]any)
	if task == nil || state == nil {
		t.Fatalf("inspect JSON shape = %v, want {task, state}", doc)
	}
	if task["id"] != "T1000" {
		t.Errorf("inspect task.id = %v, want T1000", task["id"])
	}
	if deps, ok := task["dependencies"].([]any); !ok || len(deps) != 0 {
		t.Errorf("inspect task.dependencies = %v, want []", task["dependencies"])
	}
	if state["status"] != "accepted" {
		t.Errorf("inspect state.status = %v, want accepted", state["status"])
	}

	// Unknown task: operational error, exit 1.
	code, _, stderr = e.runTaskCLI(t, "inspect", "T9999")
	if code != 1 || !strings.Contains(stderr, "unknown task") {
		t.Errorf("inspect unknown: code = %d, stderr = %q", code, stderr)
	}
}

func TestCLIIllegalTransitionLeavesFileUnchanged(t *testing.T) {
	e := newTaskEnv(t)
	e.seedState(t, map[string]devorchestrator.State{"T1000": devorchestrator.StateAccepted})
	before, err := os.ReadFile(e.statePath)
	if err != nil {
		t.Fatal(err)
	}
	// accept from accepted is illegal (accepted -> merged only).
	code, _, stderr := e.runTaskCLI(t, "accept", "T1000")
	if code != 1 {
		t.Fatalf("illegal accept exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "illegal state transition") {
		t.Errorf("stderr = %q, want illegal-transition error", stderr)
	}
	after, err := os.ReadFile(e.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("state file changed by a rejected transition")
	}
}

func TestCLIRejectRequiresReason(t *testing.T) {
	e := newTaskEnv(t)
	e.seedState(t, map[string]devorchestrator.State{"T1000": devorchestrator.StateRunning})

	code, _, stderr := e.runTaskCLI(t, "reject", "T1000")
	if code != 2 || !strings.Contains(stderr, "--reason") {
		t.Fatalf("reject without reason: code = %d, stderr = %q, want usage error", code, stderr)
	}
	if ts := e.readState(t, "T1000"); ts.Status != devorchestrator.StateRunning {
		t.Fatalf("state changed by rejected usage error: %s", ts.Status)
	}

	// Flag after the positional (docs/61 order) with --reason.
	code, out, stderr := e.runTaskCLI(t, "reject", "T1000", "--reason", "scope violation")
	if code != 0 {
		t.Fatalf("reject exit code = %d, stderr = %q", code, stderr)
	}
	ts := e.readState(t, "T1000")
	if ts.Status != devorchestrator.StateRejected || ts.RejectionReason != "scope violation" {
		t.Errorf("state = %+v, want rejected with reason", ts)
	}
	if !strings.Contains(out, "running -> rejected") {
		t.Errorf("reject output = %q", out)
	}

	// --reason-file form.
	e.seedState(t, map[string]devorchestrator.State{"T1001": devorchestrator.StateVerification})
	reasonFile := filepath.Join(e.dir, "rejection.md")
	if err := os.WriteFile(reasonFile, []byte("G2 failed: diff out of scope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = e.runTaskCLI(t, "reject", "T1001", "--reason-file", reasonFile)
	if code != 0 {
		t.Fatalf("reject --reason-file exit code = %d, stderr = %q", code, stderr)
	}
	ts = e.readState(t, "T1001")
	if ts.Status != devorchestrator.StateRejected || ts.RejectionReason != "G2 failed: diff out of scope\n" {
		t.Errorf("state = %+v, want rejected with file reason", ts)
	}
}

func TestCLIMergedUnblocksDependents(t *testing.T) {
	e := newTaskEnv(t)
	e.seedState(t, map[string]devorchestrator.State{"T1000": devorchestrator.StateAccepted})

	code, _, stderr := e.runTaskCLI(t, "merged", "T1000", "--run-id", "run-merge1")
	if code != 0 {
		t.Fatalf("merged exit code = %d, stderr = %q", code, stderr)
	}
	ts := e.readState(t, "T1000")
	if ts.Status != devorchestrator.StateMerged || ts.MergedAt == nil {
		t.Errorf("state = %+v, want merged with merged_at", ts)
	}

	code, out, stderr := e.runTaskCLI(t, "next", "--json")
	if code != 0 {
		t.Fatalf("next exit code = %d, stderr = %q", code, stderr)
	}
	if got := nextIDs(t, out); len(got) != 2 || got[0] != "T1001" || got[1] != "T1003" {
		t.Errorf("next after merge = %v, want [T1001 T1003]", got)
	}
}

func TestCLIReadyListAndUsage(t *testing.T) {
	e := newTaskEnv(t)
	e.seedState(t, map[string]devorchestrator.State{"T1000": devorchestrator.StateReady})

	code, out, stderr := e.runTaskCLI(t, "ready")
	if code != 0 {
		t.Fatalf("ready list exit code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(out, "T1000") {
		t.Errorf("ready list = %q, want T1000", out)
	}

	code, _, stderr = e.runTaskCLI(t, "bogus")
	if code != 2 || !strings.Contains(stderr, "unknown subcommand") {
		t.Errorf("task bogus: code = %d, stderr = %q", code, stderr)
	}

	code, _, stderr = e.runTaskCLI(t, "accept", "T1000", "extra")
	if code != 2 || !strings.Contains(stderr, "unexpected argument") {
		t.Errorf("extra positional: code = %d, stderr = %q", code, stderr)
	}
}

// TestSubprocessConcurrencyRunsTwoRealProcesses exercises the atomicity
// guarantee end to end: two `rddev task verify` processes race on the same
// state file; exactly one wins and the loser fails loudly with exit 1 (no
// silent overwrite, history has exactly one entry). (T0012: accept now runs
// the acceptance gate first, so the plain-transition race is exercised with
// verify, whose semantics are a bare running -> verification transition.)
func TestSubprocessConcurrencyRunsTwoRealProcesses(t *testing.T) {
	bin := buildRDDDev(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	dag := map[string]any{
		"version": 1,
		"tasks": []map[string]any{
			{"id": "T1000", "phase": "P1", "title": "task A", "dependencies": []string{}},
		},
	}
	writeJSONFile(t, filepath.Join(dir, "tasks", "tasks.json"), dag)
	writeJSONFile(t, filepath.Join(dir, "tasks", "task_status.json"), map[string]any{
		"version": 2,
		"tasks": map[string]any{
			"T1000": map[string]any{"status": "running"},
		},
	})

	runOne := func() (int, string, error) {
		cmd := exec.Command(bin, "task", "verify", "T1000")
		cmd.Dir = dir
		var errb bytes.Buffer
		cmd.Stderr = &errb
		if err := cmd.Run(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return ee.ExitCode(), errb.String(), nil
			}
			return 0, errb.String(), err
		}
		return 0, errb.String(), nil
	}

	start := make(chan struct{})
	codes := make(chan int, 2)
	stderrs := make(chan string, 2)
	runErrs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			c, e, err := runOne()
			codes <- c
			stderrs <- e
			runErrs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(codes)
	close(stderrs)
	close(runErrs)
	for err := range runErrs {
		if err != nil {
			t.Fatalf("running %s: %v", bin, err)
		}
	}

	wins, losses := 0, 0
	var loserErr string
	for c := range codes {
		switch c {
		case 0:
			wins++
		case 1:
			losses++
		default:
			t.Errorf("process exit code = %d, want 0 or 1", c)
		}
	}
	for e := range stderrs {
		if e != "" {
			loserErr = e
		}
	}
	if wins != 1 || losses != 1 {
		t.Fatalf("wins = %d, losses = %d, want exactly one of each", wins, losses)
	}
	if !strings.Contains(loserErr, "illegal state transition") {
		t.Errorf("loser stderr = %q, want loud illegal-transition error", loserErr)
	}

	// Exactly one committed transition, no leftover temp files.
	env := &taskEnv{dir: dir, dagPath: filepath.Join(dir, "tasks", "tasks.json"),
		statePath: filepath.Join(dir, "tasks", "task_status.json")}
	ts := env.readState(t, "T1000")
	if ts.Status != devorchestrator.StateVerification {
		t.Errorf("final status = %s, want verification", ts.Status)
	}
	if len(ts.History) != 1 {
		t.Errorf("history = %+v, want exactly one entry", ts.History)
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, "tasks", ".task-status-*.tmp"))
	if len(leftovers) != 0 {
		t.Errorf("leftover temp files: %v", leftovers)
	}
}

// buildRDDDev builds the rddev binary once per test run (the subprocess tests
// must exercise the real CLI, not the in-process runner).
var (
	buildOnce sync.Once
	buildBin  string
	buildErr  error
)

func buildRDDDev(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		if _, err := exec.LookPath("go"); err != nil {
			buildErr = fmt.Errorf("go toolchain unavailable: %w", err)
			return
		}
		dir, err := os.MkdirTemp("", "rddev-bin-*")
		if err != nil {
			buildErr = err
			return
		}
		buildBin = filepath.Join(dir, "rddev")
		cmd := exec.Command("go", "build", "-o", buildBin, ".")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Skipf("skipping subprocess test: %v", buildErr)
	}
	return buildBin
}

// TestSubprocessJSONShape checks the machine output of a real process:
// deterministic JSON fields, no stderr noise on success.
func TestSubprocessJSONShape(t *testing.T) {
	bin := buildRDDDev(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	dag := map[string]any{
		"version": 1,
		"tasks": []map[string]any{
			{"id": "T1000", "phase": "P1", "title": "task A", "dependencies": []string{}},
		},
	}
	writeJSONFile(t, filepath.Join(dir, "tasks", "tasks.json"), dag)
	writeJSONFile(t, filepath.Join(dir, "tasks", "task_status.json"), map[string]any{
		"version": 2,
		"tasks": map[string]any{"T1000": map[string]any{"status": "todo"}},
	})

	cmd := exec.Command(bin, "task", "ready", "T1000", "--json", "--run-id", "run-sub")
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("subprocess ready: %v\nstderr: %s", err, errb.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("subprocess JSON invalid: %v\n%s", err, out.String())
	}
	if doc["task_id"] != "T1000" || doc["from"] != "todo" || doc["to"] != "ready" || doc["run_id"] != "run-sub" {
		t.Errorf("subprocess JSON = %v, want deterministic transition fields", doc)
	}
	if doc["at"] == "" || doc["at"] == nil {
		t.Errorf("subprocess JSON missing at: %v", doc)
	}
	if errb.Len() != 0 {
		t.Errorf("subprocess stderr = %q, want empty", errb.String())
	}

	// Same process, next --json: no T1000 (now ready? no — next lists ready).
	cmd = exec.Command(bin, "task", "next", "--json")
	cmd.Dir = dir
	out.Reset()
	errb.Reset()
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("subprocess next: %v", err)
	}
	if !strings.Contains(out.String(), `"id":"T1000"`) || !strings.Contains(out.String(), `"status":"ready"`) {
		t.Errorf("subprocess next = %s, want T1000 ready", out.String())
	}
}

// acceptReviewGateFixture builds a scratch repo whose gate spec has one
// trivially-passing job so G2 can go green, and requires a review verdict for
// merge. It returns the repo dir and the gate spec path.
func acceptReviewGateFixture(t *testing.T, e *taskEnv) (string, string) {
	t.Helper()
	spec := map[string]any{
		"version":       1,
		"required_jobs": []string{"job-a"},
		"gates": map[string]any{
			"G1": map[string]any{"name": "worker", "description": "", "runs_jobs": []string{}},
			"G2": map[string]any{"name": "accept", "description": "", "runs_jobs": []string{"job-a"}},
			"G3": map[string]any{"name": "e2e", "description": "", "runs_jobs": []string{}},
			"G4": map[string]any{"name": "merge", "description": "", "asserts_jobs": []string{"job-a"}},
		},
		"jobs": map[string]any{
			"job-a": map[string]any{"steps": []map[string]any{{"run": "echo a-ok"}}},
		},
		"review":         map[string]any{"required_for_merge": true},
		"task_overrides": map[string]any{},
	}
	specPath := filepath.Join(e.dir, "gates.json")
	writeJSONFile(t, specPath, spec)
	// A green collect that predates the G2 run accept is about to make.
	if _, err := devorchestrator.WriteRecord(e.dir, "T1000", devorchestrator.RecordCollect, "run-collect-1", &devorchestrator.CollectRecord{
		RecordType: devorchestrator.RecordCollect, TaskID: "T1000", RunID: "run-collect-1",
		At: "2020-01-01T00:00:00Z", Status: "ok", ReportPath: "/none",
		ResultState: "verification", Summary: "fixture",
	}); err != nil {
		t.Fatal(err)
	}
	return e.dir, specPath
}

// accept must not be reachable in an order that makes a LATER gate impossible
// to satisfy. It used to hard-code G4 as "not_required" and move the task to
// accepted; `rddev pr open` then refused because a review verdict is required
// for merge, and a Review Worker may only review a task in verification. The
// task was accepted and unmergeable at the same time, with no legal transition
// out — T0101 sat exactly there.
func TestCLIAcceptRefusesWhenTheMergeGateIsUnsatisfied(t *testing.T) {
	e := newTaskEnv(t)
	e.seedState(t, map[string]devorchestrator.State{"T1000": devorchestrator.StateVerification})
	repo, specPath := acceptReviewGateFixture(t, e)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()

	code, _, stderr := e.runTaskCLI(t, "accept", "T1000", "--gates", specPath)
	if code == 0 {
		t.Fatalf("accept succeeded while the merge gate was unsatisfied (review required, no verdict) — the task would be accepted and unmergeable")
	}
	if !strings.Contains(stderr, "review") {
		t.Errorf("refusal does not name the review requirement: %q", stderr)
	}
	if got := e.readState(t, "T1000").Status; got != devorchestrator.StateVerification {
		t.Errorf("state = %s after a refused accept, want verification (a refusal must not move the task)", got)
	}
}

// The neighbour: with the same spec and a review verdict on record, accept
// must go through. Without this, the test above would pass for a check that
// simply refuses everything.
func TestCLIAcceptSucceedsOnceTheMergeGateIsSatisfied(t *testing.T) {
	e := newTaskEnv(t)
	e.seedState(t, map[string]devorchestrator.State{"T1000": devorchestrator.StateVerification})
	repo, specPath := acceptReviewGateFixture(t, e)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()

	if _, err := devorchestrator.WriteRecord(repo, "T1000", devorchestrator.RecordReview, "run-review-1", &devorchestrator.ReviewRecord{
		RecordType: devorchestrator.RecordReview, TaskID: "T1000", RunID: "run-review-1",
		At: "2030-01-01T00:00:00Z", Verdict: "approve", BlockingFindings: 0,
	}); err != nil {
		t.Fatal(err)
	}

	code, out, stderr := e.runTaskCLI(t, "accept", "T1000", "--gates", specPath)
	if code != 0 {
		t.Fatalf("accept refused with a review verdict on record: %s%s", out, stderr)
	}
	if got := e.readState(t, "T1000").Status; got != devorchestrator.StateAccepted {
		t.Errorf("state = %s, want accepted", got)
	}
}
