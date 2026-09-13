package devorchestrator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeSyntheticDAG writes a scratch DAG: A -> B -> C, plus a no-deps task D.
func writeSyntheticDAG(t *testing.T, dir string) string {
	t.Helper()
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
	path := filepath.Join(dir, "tasks.json")
	writeJSON(t, path, dag)
	return path
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func openScratch(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	dagPath := writeSyntheticDAG(t, dir)
	s, err := OpenStore(dagPath, filepath.Join(dir, "task_status.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func statusOf(t *testing.T, path, id string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return string(StateTodo)
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
	var ts TaskState
	if err := json.Unmarshal(tasks[id], &ts); err != nil {
		t.Fatal(err)
	}
	return string(ts.Status)
}

func historyLen(t *testing.T, path, id string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
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
	var ts TaskState
	if err := json.Unmarshal(tasks[id], &ts); err != nil {
		t.Fatal(err)
	}
	return len(ts.History)
}

func TestNextOnlyDependencySatisfied(t *testing.T) {
	s := openScratch(t)
	// T1000 merged -> T1001 (deps [T1000]) satisfies; T1002 (deps [T1001]) does
	// not, because T1001 is still todo. T1003 has no deps and is eligible.
	if _, err := s.Transition("T1000", StateReady, NewRunID(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition("T1000", StateRunning, NewRunID(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition("T1000", StateVerification, NewRunID(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition("T1000", StateAccepted, NewRunID(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition("T1000", StateMerged, NewRunID(), ""); err != nil {
		t.Fatal(err)
	}

	next, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, n := range next {
		ids = append(ids, n.ID)
	}
	want := []string{"T1001", "T1003"} // sorted by id; T1002's dep is unsatisfied
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("Next() ids = %v, want %v", ids, want)
	}
	for _, n := range next {
		switch n.ID {
		case "T1001":
			if !n.DepsMet || len(n.UnmetDeps) != 0 {
				t.Errorf("T1001: DepsMet=%v Unmet=%v, want satisfied", n.DepsMet, n.UnmetDeps)
			}
		case "T1003":
			if !n.DepsMet {
				t.Errorf("T1003: DepsMet=%v, want true (no deps)", n.DepsMet)
			}
		}
	}
	// The unsatisfied task must not appear at all.
	for _, n := range next {
		if n.ID == "T1002" {
			t.Fatalf("Next() returned T1002 whose dependency T1001 is not merged")
		}
	}
}

func TestEveryIllegalTransitionRejectedAndFileUnchanged(t *testing.T) {
	// The illegal set is DERIVED from the transition table, never hand-listed.
	//
	// It used to be a hand-written complement, and adding accepted -> rejected
	// left a stale row asserting that exact pair was illegal — the test failed
	// only because it happened to cover the pair. A hand-maintained complement
	// is wrong in the other direction too: forget to remove a pair and the
	// suite silently stops testing it. Deriving it means every (from, to) pair
	// is covered by construction, including pairs added later.
	allStates := []State{
		StateTodo, StateReady, StateRunning, StateWorkerFailed, StateVerification,
		StateRejected, StateBlocked, StateAccepted, StateMerged,
	}
	illegal := map[State][]State{}
	for _, from := range allStates {
		legal := map[State]bool{}
		for _, to := range transitions[from] {
			legal[to] = true
		}
		for _, to := range allStates {
			if !legal[to] {
				illegal[from] = append(illegal[from], to)
			}
		}
	}
	if len(illegal) != len(allStates) {
		t.Fatalf("derived illegal set covers %d states, want %d", len(illegal), len(allStates))
	}
	for from, targets := range illegal {
		for _, to := range targets {
			t.Run(fmt.Sprintf("%s->%s", from, to), func(t *testing.T) {
				s := openScratch(t)
				// Drive T1000 (no deps) into `from` via legal steps.
				drive(t, s, "T1000", from)
				var before []byte
				if b, err := os.ReadFile(s.StatePath()); err == nil {
					before = b
				}
				_, err := s.Transition("T1000", to, NewRunID(), "")
				if err == nil {
					t.Fatalf("transition %s -> %s succeeded, want rejection", from, to)
				}
				var ite *IllegalTransitionError
				if !asIllegal(err, &ite) {
					t.Fatalf("transition %s -> %s: error %v is not an IllegalTransitionError", from, to, err)
				}
				if !strings.Contains(err.Error(), string(from)) || !strings.Contains(err.Error(), string(to)) {
					t.Errorf("error %q does not name both states", err)
				}
				var after []byte
				if b, err := os.ReadFile(s.StatePath()); err == nil {
					after = b
				}
				if string(before) != string(after) {
					t.Fatalf("state file changed by a rejected transition:\nbefore: %q\nafter:  %q", before, after)
				}
				if got := statusOf(t, s.StatePath(), "T1000"); got != string(from) {
					t.Fatalf("status = %s after rejected transition, want %s", got, from)
				}
			})
		}
	}
}

// TestRejectedToRunningLegalIsTheReworkRespawnPath: T0012 makes
// rejected -> running the rework/respawn transition (the state machine no
// longer detours a rejected task through ready). The transition records a
// fresh worker run and the rejection reason stays on the state.
func TestRejectedToRunningLegalIsTheReworkRespawnPath(t *testing.T) {
	s := openScratch(t)
	drive(t, s, "T1000", StateVerification)
	if _, err := s.Transition("T1000", StateRejected, NewRunID(), "G2 red: staticcheck fails"); err != nil {
		t.Fatal(err)
	}
	res, err := s.Transition("T1000", StateRunning, NewRunID(), "rework dispatched")
	if err != nil {
		t.Fatalf("rejected -> running must be legal (T0012 rework/respawn): %v", err)
	}
	if res.From != StateRejected || res.To != StateRunning {
		t.Errorf("transition = %s -> %s, want rejected -> running", res.From, res.To)
	}
	if got := statusOf(t, s.StatePath(), "T1000"); got != string(StateRunning) {
		t.Fatalf("status = %s, want running", got)
	}
}

// asErr reports whether err is or wraps an error of type *T.
func asErr[T error](err error, target *T) bool {
	for err != nil {
		if e, ok := err.(T); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func asIllegal(err error, target **IllegalTransitionError) bool { return asErr(err, target) }

// drive moves id through legal transitions until it reaches want.
func drive(t *testing.T, s *Store, id string, want State) {
	t.Helper()
	step := func(to State) {
		t.Helper()
		if _, err := s.Transition(id, to, NewRunID(), ""); err != nil {
			t.Fatalf("driving %s -> %s: %v", id, to, err)
		}
	}
	switch want {
	case StateTodo:
		return // absent status == todo
	case StateReady:
		step(StateReady)
	case StateBlocked:
		step(StateBlocked) // todo -> blocked is legal
	case StateRunning:
		step(StateReady)
		step(StateRunning)
	case StateVerification:
		step(StateReady)
		step(StateRunning)
		step(StateVerification)
	case StateAccepted:
		step(StateReady)
		step(StateRunning)
		step(StateVerification)
		step(StateAccepted)
	case StateMerged:
		step(StateReady)
		step(StateRunning)
		step(StateVerification)
		step(StateAccepted)
		step(StateMerged)
	case StateWorkerFailed:
		step(StateReady)
		step(StateRunning)
		step(StateWorkerFailed)
	case StateRejected:
		step(StateReady)
		step(StateRunning)
		step(StateRejected)
	default:
		t.Fatalf("drive: unknown target state %s", want)
	}
}

func TestReadyRequiresDependenciesMerged(t *testing.T) {
	s := openScratch(t)
	// T1001 depends on T1000 which is still todo: marking ready must fail.
	_, err := s.Transition("T1001", StateReady, NewRunID(), "")
	if err == nil {
		t.Fatal("ready accepted with unsatisfied dependency")
	}
	var de *DependencyError
	if !asErr[*DependencyError](err, &de) {
		t.Fatalf("error %v is not a DependencyError", err)
	}
	if !strings.Contains(err.Error(), "T1000") {
		t.Fatalf("error %q does not name the unmet dependency", err)
	}
	if _, statErr := os.Stat(s.StatePath()); !os.IsNotExist(statErr) {
		t.Fatalf("state file was created by a rejected transition")
	}

	// Once T1000 is merged, the same transition succeeds.
	if _, err := s.Transition("T1000", StateReady, NewRunID(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition("T1000", StateRunning, NewRunID(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition("T1000", StateVerification, NewRunID(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition("T1000", StateAccepted, NewRunID(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition("T1000", StateMerged, NewRunID(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition("T1001", StateReady, NewRunID(), ""); err != nil {
		t.Fatalf("ready after dependency merged: %v", err)
	}
}

func TestTransitionCarriesRunIDInHistory(t *testing.T) {
	s := openScratch(t)
	res, err := s.Transition("T1003", StateReady, "run-abc123", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.RunID != "run-abc123" || res.From != StateTodo || res.To != StateReady {
		t.Fatalf("result = %+v", res)
	}
	data, err := os.ReadFile(s.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatal(err)
	}
	tasks := map[string]json.RawMessage{}
	if err := json.Unmarshal(top["tasks"], &tasks); err != nil {
		t.Fatal(err)
	}
	var ts TaskState
	if err := json.Unmarshal(tasks["T1003"], &ts); err != nil {
		t.Fatal(err)
	}
	if len(ts.History) != 1 {
		t.Fatalf("history length = %d, want 1", len(ts.History))
	}
	h := ts.History[0]
	if h.RunID != "run-abc123" || h.From != StateTodo || h.To != StateReady || h.At == "" {
		t.Fatalf("history entry = %+v", h)
	}
	// The untouched task entries must survive byte-for-byte.
	if _, err := s.Transition("T1003", StateRunning, "run-def456", ""); err != nil {
		t.Fatal(err)
	}
	if got := historyLen(t, s.StatePath(), "T1003"); got != 2 {
		t.Fatalf("history length after second transition = %d, want 2", got)
	}
}

func TestUnknownFieldsPreservedOnMutation(t *testing.T) {
	// A Supervisor-added field that rddev does not model must survive a state
	// change of the same task, and untouched tasks must pass through verbatim.
	s := openScratch(t)
	// Seed the state file directly with a foreign field on T1003.
	seed := map[string]any{
		"version": 2,
		"tasks": map[string]any{
			"T1003": map[string]any{
				"status":               "todo",
				"supervisor_owned_tag": "keep-me",
				"nested":               map[string]any{"a": 1},
			},
		},
	}
	writeJSON(t, s.StatePath(), seed)

	before, err := os.ReadFile(s.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition("T1003", StateReady, NewRunID(), ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatal(err)
	}
	tasks := map[string]json.RawMessage{}
	if err := json.Unmarshal(top["tasks"], &tasks); err != nil {
		t.Fatal(err)
	}
	var entry map[string]any
	if err := json.Unmarshal(tasks["T1003"], &entry); err != nil {
		t.Fatal(err)
	}
	if entry["supervisor_owned_tag"] != "keep-me" {
		t.Fatalf("foreign scalar field lost: %v", entry)
	}
	if nested, ok := entry["nested"].(map[string]any); !ok || nested["a"] != float64(1) {
		t.Fatalf("foreign nested field lost: %v", entry["nested"])
	}
	if string(before) == string(data) {
		t.Fatal("state file did not change at all")
	}
}

func TestAcceptSetsAcceptedAtAndRejectRecordsReason(t *testing.T) {
	s := openScratch(t)
	drive(t, s, "T1003", StateVerification)
	if _, err := s.Transition("T1003", StateAccepted, NewRunID(), ""); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(s.StatePath())
	var top map[string]json.RawMessage
	json.Unmarshal(data, &top)
	tasks := map[string]json.RawMessage{}
	json.Unmarshal(top["tasks"], &tasks)
	var ts TaskState
	json.Unmarshal(tasks["T1003"], &ts)
	if ts.AcceptedBySupervisorAt == nil || *ts.AcceptedBySupervisorAt == "" {
		t.Fatalf("accepted_by_supervisor_at not set: %+v", ts)
	}

	s2 := openScratch(t)
	drive(t, s2, "T1003", StateRunning)
	if _, err := s2.Transition("T1003", StateRejected, NewRunID(), "scope violation"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(s2.StatePath())
	json.Unmarshal(data, &top)
	json.Unmarshal(top["tasks"], &tasks)
	json.Unmarshal(tasks["T1003"], &ts)
	if ts.RejectionReason != "scope violation" {
		t.Fatalf("rejection_reason = %q", ts.RejectionReason)
	}

	// accepted -> merged stamps merged_at and leaves accepted_at intact.
	if _, err := s.Transition("T1003", StateMerged, NewRunID(), ""); err != nil {
		t.Fatal(err)
	}
	res, err := s.Inspect("T1003")
	if err != nil {
		t.Fatal(err)
	}
	if res.State.MergedAt == nil || *res.State.MergedAt == "" {
		t.Fatalf("merged_at not set: %+v", res.State)
	}
	if res.State.AcceptedBySupervisorAt == nil {
		t.Fatalf("accepted_by_supervisor_at lost after merged: %+v", res.State)
	}
}

// TestPersistedReasonsCarryNoCredentialAndLoseNoText: a reason is written into
// tasks/task_status.json, which is committed to the repository, so it is an
// output path — and the text is composed elsewhere, by whichever check failed.
// A reason that carries a Worker's inline environment verbatim puts a DSN in
// git permanently; a reason redacted into "***" deletes the audit trail the
// reason exists to be.
//
// Both halves are asserted against the FILE rather than the returned value,
// because the file is the artifact that gets committed.
func TestPersistedReasonsCarryNoCredentialAndLoseNoText(t *testing.T) {
	s := openScratch(t)
	drive(t, s, "T1003", StateRunning)

	const credential = "hunter2"
	reason := "G2 failed: out of scope internal/foo/bar.go\n" +
		"command: POSTGRES_TEST_ADMIN_URL=postgres://postgres:" + credential + "@127.0.0.1:5432/post go test -count=1 ./tests/integration/\n" +
		"ref " + strings.Repeat("a", 40)

	if _, err := s.Transition("T1003", StateRejected, NewRunID(), reason); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(credential)) {
		t.Errorf("the credential reached the committed state file:\n%s", raw)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	var tasks map[string]json.RawMessage
	if err := json.Unmarshal(top["tasks"], &tasks); err != nil {
		t.Fatal(err)
	}
	var ts TaskState
	if err := json.Unmarshal(tasks["T1003"], &ts); err != nil {
		t.Fatal(err)
	}

	// Not "***": the reason's own words are still there to be read.
	if ts.RejectionReason == "***" || ts.RejectionReason == "" {
		t.Fatalf("the reason was deleted rather than redacted: %q", ts.RejectionReason)
	}
	for _, must := range []string{
		"G2 failed: out of scope internal/foo/bar.go",
		"postgres://postgres:***@127.0.0.1:5432/post",
		"go test -count=1 ./tests/integration/",
	} {
		if !strings.Contains(ts.RejectionReason, must) {
			t.Errorf("the persisted reason lost %q:\n%s", must, ts.RejectionReason)
		}
	}
	if !strings.Contains(ts.RejectionReason, "\n") {
		t.Errorf("the persisted reason lost its line structure: %q", ts.RejectionReason)
	}
	// The one loss this design accepts, asserted so it is a decision rather
	// than a surprise: a 40+ character opaque run is masked, because the rule
	// that catches a bare token cannot tell it from a commit sha.
	if !strings.Contains(ts.RejectionReason, "ref ***") {
		t.Errorf("expected the long opaque run to be masked, got:\n%s", ts.RejectionReason)
	}
	// History carries the same text, so it must be redacted by the same rule —
	// a leak in the history entry would be committed just the same.
	if n := len(ts.History); n == 0 {
		t.Fatal("no history entry recorded")
	} else if last := ts.History[n-1]; strings.Contains(last.Reason, credential) {
		t.Errorf("the credential reached the history entry: %q", last.Reason)
	}
}

func TestConcurrentWritersDoNotSilentlyOverwrite(t *testing.T) {
	// Two writers race the same transition on the same file. The store
	// serializes them on the exclusive lock; exactly one commits and the other
	// must fail loudly on the now-illegal transition — never silently
	// overwrite. Exercised for real (goroutines + separate lock fds), not
	// asserted from reading the code.
	s := openScratch(t)
	drive(t, s, "T1003", StateVerification)

	const writers = 8
	start := make(chan struct{})
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = s.Transition("T1003", StateAccepted, fmt.Sprintf("run-w%d", i), "")
		}()
	}
	close(start)
	wg.Wait()

	successes, failures := 0, 0
	for _, err := range errs {
		if err == nil {
			successes++
		} else {
			failures++
			var ite *IllegalTransitionError
			if !asIllegal(err, &ite) {
				t.Fatalf("losing writer failed with non-transition error: %v", err)
			}
		}
	}
	if successes != 1 || failures != writers-1 {
		t.Fatalf("successes=%d failures=%d, want exactly 1 success and %d loud failures", successes, failures, writers-1)
	}
	if got := statusOf(t, s.StatePath(), "T1003"); got != string(StateAccepted) {
		t.Fatalf("final status = %s, want accepted", got)
	}
	// Exactly one state change may have been committed by the race itself.
	if got := historyLen(t, s.StatePath(), "T1003"); got != 4 {
		t.Fatalf("history length = %d, want 3 driving entries + exactly 1 accepted entry", got)
	}
	// The file must still be valid JSON and the store fully operational.
	if _, err := s.Next(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentIndependentUpdatesAllCommit(t *testing.T) {
	// Distinct tasks updated concurrently: the lock must serialize without
	// losing any of the independent updates.
	s := openScratch(t)
	start := make(chan struct{})
	ids := []string{"T1003", "T1000"} // both dependency-free
	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = s.Transition(id, StateReady, NewRunID(), "")
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("independent update %d failed: %v", i, err)
		}
	}
	for _, id := range ids {
		if got := statusOf(t, s.StatePath(), id); got != string(StateReady) {
			t.Fatalf("%s = %s after concurrent update, want ready", id, got)
		}
	}
}

func TestRealDAGLoadsAndIsConsistent(t *testing.T) {
	dagPath := filepath.Join("..", "..", "tasks", "tasks.json")
	d, err := LoadDAG(dagPath)
	if err != nil {
		t.Fatal(err) // the in-repo DAG must always parse and reference only known tasks
	}
	if len(d.Tasks) == 0 {
		t.Fatal("DAG is empty")
	}
	t9 := d.Get("T0009")
	if t9 == nil {
		t.Fatal("T0009 missing from DAG")
	}
	if len(t9.Dependencies) == 0 {
		t.Fatal("T0009 declares no dependencies")
	}
	// Real state file: statuses must all be known states.
	s, err := OpenStore(dagPath, filepath.Join("..", "..", "tasks", "task_status.json"))
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	// Independent oracle: every returned task must have deps all merged per
	// the real status file, and only todo/ready tasks are returned.
	for _, n := range next {
		if !n.DepsMet {
			t.Errorf("Next() returned %s with unmet deps %v", n.ID, n.UnmetDeps)
		}
		if n.Status != StateTodo && n.Status != StateReady {
			t.Errorf("Next() returned %s with status %s", n.ID, n.Status)
		}
	}
}

func TestEmptyPoolSerializesAsEmptyArray(t *testing.T) {
	s := openScratch(t)
	// Drive the two dispatchable tasks out of the pool (ready -> running) so
	// the pool is empty; it must still marshal as [], never null.
	for _, id := range []string{"T1000", "T1003"} {
		for _, to := range []State{StateReady, StateRunning} {
			if _, err := s.Transition(id, to, NewRunID(), ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	next, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	if next == nil || len(next) != 0 {
		t.Fatalf("Next() = %#v, want non-nil empty pool", next)
	}
	data, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "[]" {
		t.Fatalf("Next() marshals to %s, want []", data)
	}
	ready, err := s.ReadyList()
	if err != nil {
		t.Fatal(err)
	}
	if ready == nil || len(ready) != 0 {
		t.Fatalf("ReadyList() = %#v, want non-nil empty pool", ready)
	}
}

func TestInspectReturnsSpecAndState(t *testing.T) {
	s := openScratch(t)

	// No state file yet: the implicit todo state with no history.
	got, err := s.Inspect("T1001")
	if err != nil {
		t.Fatal(err)
	}
	if got.Task.ID != "T1001" || got.Task.Title != "task B" {
		t.Errorf("inspect task = %+v, want T1001 spec", got.Task)
	}
	if got.State.Status != StateTodo {
		t.Errorf("inspect state = %s, want todo", got.State.Status)
	}
	if len(got.State.History) != 0 {
		t.Errorf("inspect history = %d entries, want 0", len(got.State.History))
	}
	if got.Task.Dependencies[0] != "T1000" {
		t.Errorf("inspect dependencies = %v", got.Task.Dependencies)
	}

	// After driving to accepted, inspect reflects status, history, run_id and
	// the accepted_at stamp.
	drive(t, s, "T1000", StateAccepted)
	got, err = s.Inspect("T1000")
	if err != nil {
		t.Fatal(err)
	}
	if got.State.Status != StateAccepted {
		t.Errorf("inspect status = %s, want accepted", got.State.Status)
	}
	if len(got.State.History) != 4 { // ready, running, verification, accepted
		t.Errorf("inspect history = %d entries, want 4", len(got.State.History))
	}
	last := got.State.History[len(got.State.History)-1]
	if last.To != StateAccepted || last.RunID == "" || last.At == "" {
		t.Errorf("last history entry = %+v, want accepted with run_id and at", last)
	}
	if got.State.AcceptedBySupervisorAt == nil {
		t.Error("accepted_by_supervisor_at not stamped")
	}

	// Unknown id is an error.
	if _, err := s.Inspect("T9999"); err == nil {
		t.Error("inspect of unknown task succeeded")
	}
}

// The task state's timestamps are validated in CI
// (scripts/validate_task_state.py, ISO_TS_RE: no fractional seconds) and the
// run start spawn hands the store carries nanoseconds, because a start to the
// second cannot order a verdict written inside the same second (#105). Both
// have to hold at once, and they hold in different files: the run records keep
// the nanosecond start (collect compares the registry against the gate inputs,
// never against this), and tasks/task_status.json keeps the shape the validator
// accepts. This pins the second half through the one place that writes it —
// reverting the store's rendering leaves the state file unusable to CI on the
// next spawn, which the round-1 review measured directly (the validator exits
// 1 on a ns-shaped started_at).
func TestStartWorkerFromStampsTheTaskStateInItsValidatedFormat(t *testing.T) {
	dir := t.TempDir()
	statusPath := filepath.Join(dir, "task_status.json")
	s, err := OpenStore(writeSyntheticDAG(t, dir), statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition("T1000", StateReady, NewRunID(), ""); err != nil {
		t.Fatal(err)
	}

	// The shape spawn actually passes in, from the same helper spawn uses.
	runStart := runStartedAtFrom(time.Date(2026, 9, 13, 11, 51, 44, 877690809, time.UTC))
	if runStart != "2026-09-13T11:51:44.877690809Z" {
		t.Fatalf("fixture run start = %q, want the nanosecond form spawn records", runStart)
	}

	res, err := s.StartWorkerFrom("T1000", "run-ns", runStart, StateReady)
	if err != nil {
		t.Fatal(err)
	}

	var raw struct {
		Tasks map[string]TaskState `json:"tasks"`
	}
	data, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	ts := raw.Tasks["T1000"]
	if ts.StartedAt == nil {
		t.Fatal("started_at not stamped")
	}
	if len(ts.History) == 0 {
		t.Fatal("no history entry for the running transition")
	}

	const want = "2026-09-13T11:51:44Z"
	for _, c := range []struct{ what, got string }{
		{"the transition result's at", res.At},
		{"started_at", *ts.StartedAt},
		{"the history entry's at", ts.History[len(ts.History)-1].At},
	} {
		if c.got != want {
			t.Errorf("%s = %q, want %q — the format scripts/validate_task_state.py enforces on this file", c.what, c.got, want)
		}
	}

	// A second-precision start is passed through unchanged: the store renders
	// the task state's format, it does not round every caller's value to
	// whatever it likes.
	if _, err := s.Transition("T1003", StateReady, NewRunID(), ""); err != nil {
		t.Fatal(err)
	}
	res, err = s.StartWorkerFrom("T1003", "run-s", "2026-09-13T00:00:00Z", StateReady)
	if err != nil {
		t.Fatal(err)
	}
	if res.At != "2026-09-13T00:00:00Z" {
		t.Errorf("a second-precision start came back as %q, want it unchanged", res.At)
	}
}
