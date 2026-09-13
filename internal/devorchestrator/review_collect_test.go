package devorchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	writeReviewAttemptAt(t, repoRoot, taskID, fingerprint, taskRec, "2026-09-12T12:00:00Z")
}

// writeReviewAttemptAt is writeReviewAttempt with the run's recorded start
// time under the caller's control: the freshness check compares a verdict
// file's mtime with it, and a test of that comparison has to place both inside
// the same second — which is where the check used to fail.
func writeReviewAttemptAt(t *testing.T, repoRoot, taskID, fingerprint string, taskRec *WorkerRecord, startedAt string) {
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
		StartedAt: startedAt, ExitStatus: &exited, ExitSource: ExitSourceReaper,
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
	verdict := `{"task_id":"` + taskID + `","verdict":"approve","summary":"the change is correct","findings":[],"risks":[]}`
	path := filepath.Join(WorkerTaskDir(repoRoot, ReviewTaskID(taskID)), "RESULT.json")
	if err := os.WriteFile(path, []byte(verdict), 0o644); err != nil {
		t.Fatal(err)
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

// A verdict written before its own run started belongs to an earlier attempt
// even when the two times fall inside the same second — and on a fast machine
// they do.
//
// This is the rejection-retry e2e's "put the old verdict back with its
// original mtime" case, made deterministic. That e2e fails only in CI, where
// the whole attempt sequence (spawn, replay, collect) runs inside one second:
// with the run's start recorded to the second (time.RFC3339), 44.500 was
// compared against 44.000, `Before` said no, and the stale approve was
// recorded as the new run's verdict. On a slower machine the two straddle a
// second boundary and the e2e passes over the same defect — so the e2e is not
// a pin, this is.
func TestReviewCollectRefusesAVerdictWrittenBeforeItsRunInTheSameSecond(t *testing.T) {
	taskID := "T0105"
	root, taskRec, _, fp := taskFixture(t, taskID)
	writtenAt := time.Date(2026, 9, 13, 11, 51, 44, 500_000_000, time.UTC)
	startedAt := time.Date(2026, 9, 13, 11, 51, 44, 900_000_000, time.UTC)
	if startedAt.Truncate(time.Second) != writtenAt.Truncate(time.Second) {
		t.Fatalf("fixture is not the case under test: %s and %s are not in the same second", writtenAt, startedAt)
	}
	writeReviewAttemptAt(t, root, taskID, fp, taskRec, runStartedAtFrom(startedAt))
	writeVerdict(t, root, taskID)
	verdict := filepath.Join(WorkerTaskDir(root, ReviewTaskID(taskID)), "RESULT.json")
	if err := os.Chtimes(verdict, writtenAt, writtenAt); err != nil {
		t.Fatal(err)
	}

	report := collectReview(t, root, taskID)
	if report.Status != "rejected" {
		t.Fatalf("collect accepted a verdict written %s, before its run started %s: %s %v",
			writtenAt.Format(time.RFC3339Nano), startedAt.Format(time.RFC3339Nano), report.Status, report.Reasons)
	}
	status, detail := checkDetail(report, "review-verdict-run")
	if status != "failed" {
		t.Fatalf("review-verdict-run = %q (%s), want failed", status, detail)
	}
	// The refusal has to name the run the verdict does not belong to, and both
	// times: "it is stale" without saying when it was written and when the run
	// started is not something a Supervisor can act on.
	if !strings.Contains(detail, "2026-09-13T11:51:44.5") || !strings.Contains(detail, "2026-09-13T11:51:44.9") {
		t.Errorf("the refusal does not name the verdict's mtime and the run's start: %s", detail)
	}
	if rec, ok, err := LatestRecord[ReviewRecord](root, taskID, RecordReview); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Errorf("an earlier attempt's verdict became this run's merge evidence: %+v", rec)
	}
}

// The control for the test above: same second, same check, and the verdict is
// written AFTER the run started — the ordinary case, which must still be
// accepted. Without it, "same second is refused" would also be satisfied by a
// check that refuses everything in the second a run starts.
func TestReviewCollectAcceptsAVerdictWrittenAfterItsRunInTheSameSecond(t *testing.T) {
	taskID := "T0106"
	root, taskRec, _, fp := taskFixture(t, taskID)
	startedAt := time.Date(2026, 9, 13, 11, 51, 44, 500_000_000, time.UTC)
	writtenAt := time.Date(2026, 9, 13, 11, 51, 44, 900_000_000, time.UTC)
	writeReviewAttemptAt(t, root, taskID, fp, taskRec, runStartedAtFrom(startedAt))
	writeVerdict(t, root, taskID)
	verdict := filepath.Join(WorkerTaskDir(root, ReviewTaskID(taskID)), "RESULT.json")
	if err := os.Chtimes(verdict, writtenAt, writtenAt); err != nil {
		t.Fatal(err)
	}

	report := collectReview(t, root, taskID)
	if report.Status != "ok" {
		t.Fatalf("collect refused this run's own verdict, written %s after it started %s: %s %v",
			writtenAt.Format(time.RFC3339Nano), startedAt.Format(time.RFC3339Nano), report.Status, report.Reasons)
	}
	if status, detail := checkDetail(report, "review-verdict-run"); status != "" {
		t.Errorf("review-verdict-run = %q (%s), want the check to say nothing about a verdict that belongs", status, detail)
	}
}
