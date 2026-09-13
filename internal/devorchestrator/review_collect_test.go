package devorchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Review collect is where a verdict BECOMES merge evidence: the ReviewRecord it
// writes is what the merge gate reads, and nothing downstream re-asks which
// code the verdict describes. The check that the reviewed tree still matches
// the fingerprint taken at spawn is therefore the whole guarantee — which made
// the fail-closed branch for an EMPTY fingerprint load-bearing, and untested.
//
// An adversarial review demonstrated the gap by mutation: reverting the empty
// case to its old form ("a review with no fingerprint is not a mismatch, so it
// passes") left the entire unit suite and the rejection-retry e2e green. A
// future edit could silently restore a green light over an unasked question.
//
// These tests pin both halves: the empty fingerprint is refused, and the
// fixture's other checks all pass, so the refusal is caused by that and nothing
// else.

// writeReviewAttempt records a dispatched, exited Review Worker the way spawn
// does: a registry entry plus the authoritative gate-inputs document. The two
// must agree or collect refuses them as tampering before it reaches the
// fingerprint, and a test that failed there would be testing the fixture.
func writeReviewAttempt(t *testing.T, repoRoot, taskID, fingerprint string, taskRec *WorkerRecord) {
	t.Helper()
	reviewID := ReviewTaskID(taskID)
	resultDir := WorkerTaskDir(repoRoot, reviewID)
	if err := os.MkdirAll(resultDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exited := 0
	rec := &WorkerRecord{
		TaskID: reviewID, RunID: "run-review-" + taskID, SessionID: "session-review",
		ClaudeVersion: "v", PID: 4242, StartTime: 99,
		Worktree: taskRec.Worktree, Branch: taskRec.Branch, BaselineSHA: taskRec.BaselineSHA,
		RefsBefore: []string{}, ListenersBefore: []string{},
		LogPath: filepath.Join(resultDir, "worker.log"), ResultDir: resultDir,
		StartedAt: "2026-09-12T12:00:00Z", ExitStatus: &exited, ExitSource: ExitSourceReaper,
	}
	if err := SaveRegistry(repoRoot, rec); err != nil {
		t.Fatal(err)
	}
	gate := &GateInputs{
		TaskID: reviewID, RunID: rec.RunID, SessionID: rec.SessionID,
		BaselineSHA: rec.BaselineSHA, Branch: rec.Branch, Worktree: rec.Worktree,
		ResultDir: rec.ResultDir, LogPath: rec.LogPath, RefsBefore: rec.RefsBefore,
		PID: rec.PID, StartTime: rec.StartTime, SessionLeaderPID: rec.SessionLeaderPID,
		ListenersBefore: rec.ListenersBefore, StartedAt: rec.StartedAt,
		ReviewDiffSHA: fingerprint,
	}
	if err := os.MkdirAll(RuntimeTasksDir(repoRoot, reviewID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(gateInputsPath(repoRoot, reviewID), marshalIndentBytes(gate)); err != nil {
		t.Fatal(err)
	}
}

// writeVerdict puts an approving verdict where collect looks for it, and the
// REAL schema where it validates it. The schema is read from the repository
// rather than copied into the fixture: a copy would let this test and the
// document the product validates against drift apart, and the test would then
// be pinning a rule nothing enforces. `go test` runs with the package
// directory as the working directory.
func writeVerdict(t *testing.T, repoRoot, taskID string) {
	t.Helper()
	writeVerdictDoc(t, repoRoot, taskID, `{"task_id":"`+taskID+`","verdict":"approve","summary":"the change is correct","findings":[],"risks":[]}`)
}

// writeVerdictDoc is writeVerdict with the document as a parameter, for the
// cases that are about what the document CONTAINS rather than about the
// fingerprint. Same real schema in the same place, so a verdict that collects
// here is one the product would collect.
func writeVerdictDoc(t *testing.T, repoRoot, taskID, verdict string) {
	t.Helper()
	schemaSrc := filepath.Join("..", "..", "specs", "orchestrator", "review-verdict.schema.json")
	schema, err := os.ReadFile(schemaSrc)
	if err != nil {
		t.Fatalf("reading the real review verdict schema (%s): %v", schemaSrc, err)
	}
	schemaDir := filepath.Join(repoRoot, "specs", "orchestrator")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(schemaDir, "review-verdict.schema.json"), schema, 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(WorkerTaskDir(repoRoot, ReviewTaskID(taskID)), "RESULT.json")
	if err := os.WriteFile(path, []byte(verdict), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestReviewCollectAcceptsAVerdictWithAnUnanchoredFinding drives the T0215
// document through the path that refused it. The unit test beside the validator
// pins the keyword's scope; this pins the WIRING — that the schema collect reads
// is the repository's, that a nil line survives the file, the parse and the
// check, and that the verdict is recorded rather than rejected. Every other
// fixture in this file has an empty findings array, so before this no verdict
// carrying a finding had ever been collected here at all.
func TestReviewCollectAcceptsAVerdictWithAnUnanchoredFinding(t *testing.T) {
	taskID := "T0103"
	root, taskRec, _, fp := taskFixture(t, taskID)
	writeReviewAttempt(t, root, taskID, fp, taskRec)
	// "This finding is not line-specific", as the schema spells it — plus the
	// inclusive boundary, because line 0 is a line and must not be confused with
	// absent.
	writeVerdictDoc(t, root, taskID, `{"task_id":"`+taskID+`","verdict":"approve","summary":"s",
		"findings":[
			{"severity":"nit","file":"RESULT.json","line":null,"finding":"not line-specific"},
			{"severity":"nit","file":"RESULT.json","line":0,"finding":"about the first line"}],
		"risks":[]}`)

	report := collectReview(t, root, taskID)
	// The schema check is only recorded when it REFUSES — passing it falls
	// through to the verdict checks below — so a failed entry is the whole
	// signal, and the walk continuing past it is the rest.
	if status, detail := checkDetail(report, "review-verdict-schema"); status == "failed" {
		t.Fatalf("the repository's schema refused the T0215 document: %s; ran: %s", detail, describeChecks(report))
	}
	if report.Status != "ok" {
		t.Fatalf("collect status = %q, want ok: %v (%s)", report.Status, report.Reasons, describeChecks(report))
	}
	if status, detail := checkDetail(report, "review-verdict"); status != "passed" {
		t.Fatalf("review-verdict = %q (%s), want passed — the verdict was not walked; ran: %s", status, detail, describeChecks(report))
	}
	// And the point of collecting at all: the document became merge evidence.
	rec, ok, err := LatestRecord[ReviewRecord](root, taskID, RecordReview)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("an accepted verdict with an unanchored finding was not recorded as merge evidence; ran: %s", describeChecks(report))
	}
	if rec.Verdict != "approve" {
		t.Errorf("recorded verdict = %q, want approve", rec.Verdict)
	}
}

func collectReview(t *testing.T, repoRoot, taskID string) *ReviewCollectReport {
	t.Helper()
	report, err := CollectReview(&CollectOpts{RepoRoot: repoRoot, TaskID: taskID, RunID: "collect-1"})
	if err != nil {
		t.Fatalf("collecting the review: %v", err)
	}
	return report
}

// describeChecks renders the checks that ran, in order, for a failure message.
func describeChecks(report *ReviewCollectReport) string {
	var b strings.Builder
	for i, c := range report.Checks {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s=%s", c.Name, c.Status)
		if c.Detail != "" {
			fmt.Fprintf(&b, "(%s)", c.Detail)
		}
	}
	if b.Len() == 0 {
		return "no checks ran"
	}
	return b.String()
}

// checkDetail returns the detail of one named collect check.
func checkDetail(report *ReviewCollectReport, name string) (string, string) {
	for _, c := range report.Checks {
		if c.Name == name {
			return c.Status, c.Detail
		}
	}
	return "", ""
}

// The control. Everything about this fixture is the same as the test below
// except the fingerprint, and collect has to accept it — that is what makes the
// refusal below a statement about the fingerprint rather than about a fixture
// that is broken somewhere else.
func TestReviewCollectAcceptsAVerdictBoundToTheReviewedCode(t *testing.T) {
	taskID := "T0103"
	root, taskRec, _, fp := taskFixture(t, taskID)
	writeReviewAttempt(t, root, taskID, fp, taskRec)
	writeVerdict(t, root, taskID)

	report := collectReview(t, root, taskID)
	if report.Status != "ok" {
		t.Fatalf("collect refused a verdict bound to the reviewed code: %s, %v", report.Status, report.Reasons)
	}
	if status, detail := checkDetail(report, "review-code-unchanged"); status != "passed" {
		t.Errorf("review-code-unchanged = %q (%s), want passed", status, detail)
	}
	if _, ok, err := LatestRecord[ReviewRecord](root, taskID, RecordReview); err != nil || !ok {
		t.Errorf("an accepted verdict was not recorded as merge evidence (ok=%v, err=%v)", ok, err)
	}
}

// The defect: no fingerprint in the spawn record. This is not "the code
// matches" — it is a review that cannot say what it reviewed, and collect must
// refuse it rather than let it become the record the merge gate reads.
func TestReviewCollectRefusesAVerdictWithNoSpawnFingerprint(t *testing.T) {
	taskID := "T0104"
	root, taskRec, _, _ := taskFixture(t, taskID)
	writeReviewAttempt(t, root, taskID, "" /* no fingerprint */, taskRec)
	writeVerdict(t, root, taskID)

	report := collectReview(t, root, taskID)
	if report.Status != "rejected" {
		t.Fatalf("a review carrying no spawn-time fingerprint was %s, and it must be rejected: %v", report.Status, report.Reasons)
	}
	status, detail := checkDetail(report, "review-code-unchanged")
	if status != "failed" {
		t.Fatalf("review-code-unchanged = %q (%s), want failed", status, detail)
	}
	// The wording matters as much as the verdict: the old branch passed this
	// case while reporting "the reviewed worktree matches the spawn-time
	// fingerprint" — a green light, in the check's own words, over a review
	// that never expressed the property.
	if !strings.Contains(detail, "no spawn-time code fingerprint") {
		t.Errorf("the refusal does not say the review carries no fingerprint: %s", detail)
	}
	if strings.Contains(detail, "matches the spawn-time fingerprint") {
		t.Errorf("the refusal claims a match it never made: %s", detail)
	}

	// The consequence, and the reason this is not cosmetic: no ReviewRecord is
	// written, so the merge gate sees "no review verdict exists" instead of a
	// verdict it would have to trust.
	if rec, ok, err := LatestRecord[ReviewRecord](root, taskID, RecordReview); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Errorf("a verdict with no fingerprint became merge evidence: %+v", rec)
	}
}
