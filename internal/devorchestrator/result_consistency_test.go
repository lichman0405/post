package devorchestrator

import (
	"os"
	"path/filepath"
	"strings"
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

// A required test is named by LABEL in the DAG ("auth unit"), while the
// contract's tests[].command holds the command that ran. T0101 ran both
// required suites and labelled them in its commands, and collect rejected it
// with "2 required test(s) have no passed entry: auth unit; auth e2e" —
// because the two strings were compared for equality. Equality was never
// satisfiable; this test pins the real requirement: the label must be named by
// a PASSED entry.
func TestRequiredTestMatchedByLabelNotEquality(t *testing.T) {
	path := writeResult(t, `{"task_id":"T0101","status":"completed","summary":"x","files_changed":[],"tests":[
		{"command":"go test ./internal/application/authn/... ./cmd/api/... -count=1 (T0101-TEST-01 auth unit, blocking)","status":"passed","evidence":"ok"},
		{"command":"go test ./tests/e2e -run TestE2E -count=1 (T0101-TEST-02 auth e2e, blocking)","status":"passed","evidence":"ok"}
	],"acceptance":[{"criterion":"c1","status":"passed","evidence":"ok"}],"risks":[],"follow_up_issues":[],"notes_for_supervisor":""}`)
	checks, consistent, err := CheckResultConsistency(path, []string{"c1"}, []string{"auth unit", "auth e2e"})
	if err != nil {
		t.Fatal(err)
	}
	if !consistent {
		st, detail := checkStatus(t, checks, "result-tests-coverage")
		t.Fatalf("a RESULT that ran and named both required suites was judged inconsistent: %s %s", st, detail)
	}
	if st, _ := checkStatus(t, checks, "result-tests-coverage"); st != "passed" {
		t.Errorf("result-tests-coverage = %s, want passed", st)
	}
}

// The looser matching must not become a hole: a required test whose only
// matching entry is not_run still fails coverage, and a required test named
// nowhere still fails it.
func TestRequiredTestCoverageStillRejectsMissingAndUnrun(t *testing.T) {
	path := writeResult(t, `{"task_id":"T0101","status":"completed","summary":"x","files_changed":[],"tests":[
		{"command":"go test ./tests/e2e -run TestE2E (T0101-TEST-02 auth e2e)","status":"not_run","evidence":"no database"}
	],"acceptance":[{"criterion":"c1","status":"passed","evidence":"ok"}],"risks":[],"follow_up_issues":[],"notes_for_supervisor":""}`)
	checks, consistent, err := CheckResultConsistency(path, []string{"c1"}, []string{"auth unit", "auth e2e"})
	if err != nil {
		t.Fatal(err)
	}
	if consistent {
		t.Fatal("a required test that is not_run, and one named nowhere, were judged covered")
	}
	st, detail := checkStatus(t, checks, "result-tests-coverage")
	if st != "failed" {
		t.Errorf("result-tests-coverage = %s, want failed", st)
	}
	// "auth e2e" is present but not_run; "auth unit" is absent entirely. Both
	// must be named as missing.
	for _, want := range []string{"auth unit", "auth e2e (not_run)"} {
		if !strings.Contains(detail, want) {
			t.Errorf("coverage detail does not name %q: %s", want, detail)
		}
	}
}

// The coverage rule needs a way for an honest Worker to state WHICH required
// test an entry satisfies. Without one it matched on a substring of a free-text
// command — a convention nobody wrote down, which T0101 failed twice while
// running both required suites both times.
func TestRequiredTestCoveredByExplicitLabel(t *testing.T) {
	path := writeResult(t, `{"task_id":"T0101","status":"completed","summary":"x","files_changed":[],"tests":[
		{"command":"go test ./internal/application/authn/ ./cmd/api/... -count=1","label":"auth unit","status":"passed","evidence":"ok"},
		{"command":"go test ./tests/e2e -count=1 -v","label":"auth e2e","status":"passed","evidence":"ok"}
	],"acceptance":[{"criterion":"c1","status":"passed","evidence":"ok"}],"risks":[],"follow_up_issues":[],"notes_for_supervisor":""}`)
	checks, consistent, err := CheckResultConsistency(path, []string{"c1"}, []string{"auth unit", "auth e2e"})
	if err != nil {
		t.Fatal(err)
	}
	if !consistent {
		st, detail := checkStatus(t, checks, "result-tests-coverage")
		t.Fatalf("entries that declare their required test were judged uncovered: %s %s", st, detail)
	}
}

// The neighbour: a label only counts when it names the requirement. An entry
// labelled something else, with the label absent from its command, still does
// not cover it — otherwise the field would be a way to satisfy the check by
// writing any string at all.
func TestRequiredTestCoverageNotSatisfiedByAnUnrelatedLabel(t *testing.T) {
	path := writeResult(t, `{"task_id":"T0101","status":"completed","summary":"x","files_changed":[],"tests":[
		{"command":"go test ./internal/application/authn/... -count=1","label":"some other suite","status":"passed","evidence":"ok"}
	],"acceptance":[{"criterion":"c1","status":"passed","evidence":"ok"}],"risks":[],"follow_up_issues":[],"notes_for_supervisor":""}`)
	checks, consistent, err := CheckResultConsistency(path, []string{"c1"}, []string{"auth unit"})
	if err != nil {
		t.Fatal(err)
	}
	if consistent {
		t.Fatal("an unrelated label was accepted as covering a required test")
	}
	if st, _ := checkStatus(t, checks, "result-tests-coverage"); st != "failed" {
		t.Errorf("result-tests-coverage = %s, want failed", st)
	}
}
