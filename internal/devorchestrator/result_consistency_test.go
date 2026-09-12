package devorchestrator

import (
	"os"
	"path/filepath"
	"testing"
)

// writeResult writes a RESULT.json fixture and returns its path.
func writeResult(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "RESULT.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func checkStatus(t *testing.T, checks []ResultConsistencyCheck, name string) (string, string) {
	t.Helper()
	for _, c := range checks {
		if c.Name == name {
			return c.Status, c.Detail
		}
	}
	t.Fatalf("no check named %q in %+v", name, checks)
	return "", ""
}

// TestResultConsistencyInterimMarkerRejected is T0012 requirement 1's core
// defect: a status: completed RESULT whose first line says "INTERIM
// SNAPSHOT" must be mechanically rejected — the marker contradicts the claim
// regardless of what the tests section says.
func TestResultConsistencyInterimMarkerRejected(t *testing.T) {
	// The shipped defect, verbatim in shape: the document's own first line
	// announces "INTERIM SNAPSHOT" while the status claims completed and the
	// required tests are not_run.
	path := writeResult(t, `{"task_id":"T0001","status":"completed","summary":"INTERIM SNAPSHOT — work in progress, not a deliverable","files_changed":["x.go"],"tests":[{"command":"go test ./...","status":"not_run","evidence":"none"},{"command":"make staticcheck","status":"not_run","evidence":"none"}],"acceptance":[{"criterion":"a1","status":"passed","evidence":"ok"}],"risks":[],"follow_up_issues":[],"notes_for_supervisor":""}`)
	checks, consistent, err := CheckResultConsistency(path, []string{"a1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if consistent {
		t.Fatal("an INTERIM-marked RESULT with not_run tests was judged consistent")
	}
	if st, _ := checkStatus(t, checks, "result-marker"); st != "failed" {
		t.Errorf("result-marker = %s, want failed", st)
	}
	if st, _ := checkStatus(t, checks, "result-tests"); st != "failed" {
		t.Errorf("result-tests = %s, want failed (not_run under completed)", st)
	}
}

// TestResultConsistencyCompletedWithNotRunRejected: a completed claim with
// not_run or failed tests is a contradiction even with no marker line.
func TestResultConsistencyCompletedWithNotRunRejected(t *testing.T) {
	path := writeResult(t, `{"task_id":"T0001","status":"completed","summary":"x","files_changed":[],"tests":[{"command":"go vet ./...","status":"not_run","evidence":"none"}],"acceptance":[{"criterion":"a1","status":"passed","evidence":"ok"}],"risks":[],"follow_up_issues":[],"notes_for_supervisor":""}`)
	checks, consistent, err := CheckResultConsistency(path, []string{"a1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if consistent {
		t.Fatal("completed with a not_run test was judged consistent")
	}
	if st, detail := checkStatus(t, checks, "result-tests"); st != "failed" || detail == "" {
		t.Errorf("result-tests = %s %q, want failed with detail", st, detail)
	}

	// failed test under completed: same contradiction.
	path = writeResult(t, `{"task_id":"T0001","status":"completed","summary":"x","files_changed":[],"tests":[{"command":"go test ./...","status":"failed","evidence":"boom"}],"acceptance":[{"criterion":"a1","status":"passed","evidence":"ok"}],"risks":[],"follow_up_issues":[],"notes_for_supervisor":""}`)
	checks, consistent, err = CheckResultConsistency(path, []string{"a1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if consistent {
		t.Fatal("completed with a failed test was judged consistent")
	}
	if st, _ := checkStatus(t, checks, "result-tests"); st != "failed" {
		t.Errorf("result-tests = %s, want failed", st)
	}
}

// TestResultConsistencyHonestNonCompletionIsNotAContradiction: failed /
// blocked statuses are honest non-completions — not consistency failures, but
// consistent=false so the caller rejects the run (the G1 gate's
// "honest non-completion is a rejected run, never verification" rule).
func TestResultConsistencyHonestNonCompletionIsNotAContradiction(t *testing.T) {
	path := writeResult(t, `{"task_id":"T0001","status":"blocked","summary":"blocker","files_changed":[],"tests":[{"command":"true","status":"passed","evidence":"ok"}],"acceptance":[{"criterion":"a1","status":"passed","evidence":"ok"}],"risks":[],"follow_up_issues":[],"notes_for_supervisor":""}`)
	checks, consistent, err := CheckResultConsistency(path, []string{"a1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if consistent {
		t.Fatal("a blocked RESULT must never be consistent (it is a rejected run)")
	}
	if st, _ := checkStatus(t, checks, "result-status"); st != "passed" {
		t.Errorf("result-status = %s, want passed (honest non-completion is not a contradiction)", st)
	}
	// Every other check passed: the document contradicts nothing.
	for _, c := range checks {
		if c.Status != "passed" {
			t.Errorf("check %s = %s, want passed for an honest non-completion", c.Name, c.Status)
		}
	}
}

// TestResultConsistencyGreenDocumentPasses: a fully green document is
// consistent — the checks must not false-positive on the happy path.
func TestResultConsistencyGreenDocumentPasses(t *testing.T) {
	path := writeResult(t, `{"task_id":"T0001","status":"completed","summary":"done","files_changed":["a.go"],"tests":[{"command":"go test ./...","status":"passed","evidence":"ok"}],"acceptance":[{"criterion":"a1","status":"passed","evidence":"ok"},{"criterion":"a2","status":"passed","evidence":"ok"}],"risks":[],"follow_up_issues":[],"notes_for_supervisor":""}`)
	checks, consistent, err := CheckResultConsistency(path, []string{"a1", "a2"}, []string{"go test ./..."})
	if err != nil {
		t.Fatal(err)
	}
	if !consistent {
		t.Fatalf("green document judged inconsistent: %+v", checks)
	}
	for _, c := range checks {
		if c.Status != "passed" {
			t.Errorf("check %s = %s, want passed", c.Name, c.Status)
		}
	}
}

// TestResultConsistencyCompletedOmittingRequiredTestRejected: the omission
// neighbour of the shipped defect — a completed RESULT that drops a required
// test from tests[] entirely (not even not_run) must not smuggle through.
// Every required test needs a passed entry with evidence.
func TestResultConsistencyCompletedOmittingRequiredTestRejected(t *testing.T) {
	path := writeResult(t, `{"task_id":"T0001","status":"completed","summary":"done","files_changed":["a.go"],"tests":[{"command":"go vet ./...","status":"passed","evidence":"ok"}],"acceptance":[{"criterion":"a1","status":"passed","evidence":"ok"}],"risks":[],"follow_up_issues":[],"notes_for_supervisor":""}`)
	checks, consistent, err := CheckResultConsistency(path, []string{"a1"}, []string{"go test ./...", "make staticcheck"})
	if err != nil {
		t.Fatal(err)
	}
	if consistent {
		t.Fatal("completed omitting both required tests was judged consistent")
	}
	st, detail := checkStatus(t, checks, "result-tests-coverage")
	if st != "failed" || detail == "" {
		t.Errorf("result-tests-coverage = %s %q, want failed naming the missing tests", st, detail)
	}
}

// TestResultConsistencyRequiredTestsCoveredPasses: the legitimate neighbour —
// a completed RESULT that covers every required test with a passed entry is
// consistent; the coverage check must not false-positive.
func TestResultConsistencyRequiredTestsCoveredPasses(t *testing.T) {
	path := writeResult(t, `{"task_id":"T0001","status":"completed","summary":"done","files_changed":["a.go"],"tests":[{"command":"go test ./...","status":"passed","evidence":"ok"},{"command":"make staticcheck","status":"passed","evidence":"ok"}],"acceptance":[{"criterion":"a1","status":"passed","evidence":"ok"}],"risks":[],"follow_up_issues":[],"notes_for_supervisor":""}`)
	checks, consistent, err := CheckResultConsistency(path, []string{"a1"}, []string{"go test ./...", "make staticcheck"})
	if err != nil {
		t.Fatal(err)
	}
	if !consistent {
		t.Fatalf("covered document judged inconsistent: %+v", checks)
	}
	st, _ := checkStatus(t, checks, "result-tests-coverage")
	if st != "passed" {
		t.Errorf("result-tests-coverage = %s, want passed", st)
	}
}

// TestResultConsistencyFewerAcceptanceEntriesThanCriteriaRejected: a
// completed RESULT must carry an entry per package criterion — silently
// dropping criteria is exactly how a Worker hides untested acceptance.
func TestResultConsistencyFewerAcceptanceEntriesThanCriteriaRejected(t *testing.T) {
	path := writeResult(t, `{"task_id":"T0001","status":"completed","summary":"done","files_changed":[],"tests":[],"acceptance":[{"criterion":"a1","status":"passed","evidence":"ok"}],"risks":[],"follow_up_issues":[],"notes_for_supervisor":""}`)
	checks, consistent, err := CheckResultConsistency(path, []string{"a1", "a2", "a3"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if consistent {
		t.Fatal("completed with 1 acceptance entry for 3 criteria was judged consistent")
	}
	if st, detail := checkStatus(t, checks, "result-acceptance"); st != "failed" || detail == "" {
		t.Errorf("result-acceptance = %s %q, want failed with detail", st, detail)
	}
}
