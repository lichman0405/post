package devorchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The workflow view's advice and the merge gate's refusal are two answers to
// one question, and the Supervisor reads the first to decide whether to take
// the second. `rddev workflow TASK` resuming from disk alone is the whole point
// of the view (T0012 requirement 7): it says "review approved — run the
// acceptance gate / rddev task accept TASK", and if the merge gate refuses that
// call, the advice sent the Supervisor into a refusal that names the command
// the advice should have given — the round trip the view exists to remove.
//
// The identity half of that question was mirrored by reviewRecordIsSuperseded.
// The OTHER refusal the merge gate makes about a review was not: a verdict
// older than the latest collect. It needs no code change to reach — a
// re-collect of the same attempt, or a rework that reproduces identical
// content, leaves the identity untouched — and an adversarial review found the
// advice and the gate disagreeing on it.
//
// This test asserts the agreement, on a fixture that is green in every other
// way, so a passing gate is what makes the stale case mean something.

const reviewRequiredGateSpec = `{
  "version": 1,
  "required_jobs": ["job-a"],
  "gates": {
    "G1": {"name": "", "description": "", "runs_jobs": [], "asserts_jobs": []},
    "G2": {"name": "", "description": "", "runs_jobs": ["job-a"], "asserts_jobs": []},
    "G3": {"name": "", "description": "", "runs_jobs": [], "asserts_jobs": []},
    "G4": {"name": "", "description": "", "runs_jobs": [], "asserts_jobs": ["job-a"]}
  },
  "jobs": {"job-a": {"steps": [{"run": "true"}]}},
  "review": {"required_for_merge": true},
  "task_overrides": {}
}`

// writeReviewRequiredSpec places a gates.json requiring a review verdict in an
// existing repo root — writeGateSpec makes its own root, and these fixtures
// need the task's worktree and registry in the same one.
func writeReviewRequiredSpec(t *testing.T, repoRoot string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repoRoot, "specs", "orchestrator"), 0o755); err != nil {
		t.Fatal(err)
	}
	specPath := filepath.Join(repoRoot, "specs", "orchestrator", "gates.json")
	if err := os.WriteFile(specPath, []byte(reviewRequiredGateSpec), 0o644); err != nil {
		t.Fatal(err)
	}
	return specPath
}

// reviewEvidenceFixture is every reason the merge gate could be red, except the
// one under test: an ok collect, an all-green G2 covering the required job
// (at/after the collect), and an approving verdict bound to the current code.
// Only the verdict's clock differs between the two cases below.
func reviewEvidenceFixture(t *testing.T, taskID, collectAt, verdictAt string) (repoRoot, specPath string, fp string) {
	t.Helper()
	repoRoot, _, _, fp = taskFixture(t, taskID)
	specPath = writeReviewRequiredSpec(t, repoRoot)

	writeCollect(t, repoRoot, taskID, "coll-2", "ok", collectAt)
	green := &GateRunRecord{
		recordMeta: recordMeta{RecordType: RecordGateRun, TaskID: taskID, RunID: "g2-1", At: nextHour(collectAt)},
		Gate:       "G2",
		Status:     "passed",
		Jobs:       []GateJobResult{{Job: "job-a", Status: "passed"}},
	}
	if _, err := WriteRecord(repoRoot, taskID, RecordGateRun, "g2-1", green); err != nil {
		t.Fatal(err)
	}
	rv := &ReviewRecord{
		recordMeta: recordMeta{RecordType: RecordReview, TaskID: taskID, RunID: "rev-1", At: verdictAt},
		Verdict:    "approve", Summary: "fixture", VerdictPath: "/none",
		DiffSHA: fp,
	}
	if _, err := WriteRecord(repoRoot, taskID, RecordReview, "rev-1", rv); err != nil {
		t.Fatal(err)
	}
	return repoRoot, specPath, fp
}

// nextHour keeps the G2 run at/after the collect without spelling out a third
// timestamp per case: the fixture is about the verdict's clock, not G2's.
func nextHour(at string) string {
	if h, ok := strings.CutSuffix(at, "13:00:00Z"); ok {
		return h + "14:00:00Z"
	}
	return "2026-09-12T14:00:00Z"
}

// adviceFor is what `rddev workflow` would print to a Supervisor resuming from
// disk: the next action and the exact command to run.
func adviceFor(t *testing.T, repoRoot, taskID string) (string, string) {
	t.Helper()
	rv, ok, err := LatestRecord[ReviewRecord](repoRoot, taskID, RecordReview)
	if err != nil || !ok {
		t.Fatalf("the fixture has no review record (ok=%v, err=%v)", ok, err)
	}
	view := &WorkflowView{
		TaskID: taskID, State: string(StateVerification),
		GateStatus: map[string]string{"G1": "passed", "G2": "passed"},
		Review:     &rv,
	}
	return nextActionFor(repoRoot, taskID, StateVerification, view)
}

// The control: a verdict written after the collect describes the tree the
// collect read, so nothing is superseded, the merge gate passes, and the advice
// says accept. Without this case the staleness assertion below could be
// satisfied by a fixture that is red for some reason nobody noticed.
func TestAdviceAndGateAgreeOnACurrentVerdict(t *testing.T) {
	taskID := "T0101"
	repoRoot, specPath, _ := reviewEvidenceFixture(t, taskID,
		"2026-09-12T12:00:00Z", "2026-09-12T13:00:00Z")

	res, err := CheckMergeGate(repoRoot, specPath, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "passed" {
		t.Fatalf("the fixture is not green: merge gate = %s, reasons %v", res.Status, res.Reasons)
	}
	action, cmd := adviceFor(t, repoRoot, taskID)
	if cmd != "rddev task accept "+taskID {
		t.Errorf("the gate passes and the advice does not say to accept: %q (%s)", cmd, action)
	}
}

// The defect: the same code identity, a collect written AFTER the verdict. The
// gate refuses ("it judged a different tree"); the advice used to say accept.
func TestAdviceAndGateAgreeWhenTheVerdictPredatesTheCollect(t *testing.T) {
	taskID := "T0102"
	repoRoot, specPath, fp := reviewEvidenceFixture(t, taskID,
		"2026-09-12T13:00:00Z", "2026-09-12T12:00:00Z")

	// The identity is unchanged — which is exactly why the identity comparison
	// alone cannot see this, and why the advice used to disagree with the gate.
	if now, err := currentCodeIdentity(repoRoot, taskID); err != nil || now != fp {
		t.Fatalf("the fixture moved the code identity (%s -> %s, err=%v); the case under test is a verdict that is older, not code that is different", fp, now, err)
	}

	res, err := CheckMergeGate(repoRoot, specPath, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status == "passed" {
		t.Fatalf("the merge gate passed a verdict older than the latest collect: %+v", res)
	}
	refusedForFreshness := false
	for _, r := range res.Reasons {
		if strings.Contains(r, "older than the latest collect") {
			refusedForFreshness = true
		}
	}
	if !refusedForFreshness {
		t.Fatalf("the gate refused, but not for the freshness of the verdict: %v — this test would then be asserting agreement about a different refusal", res.Reasons)
	}

	action, cmd := adviceFor(t, repoRoot, taskID)
	if cmd == "rddev task accept "+taskID {
		t.Errorf("the advice sends the Supervisor to the command the merge gate just refused: %q (%s)", cmd, action)
	}
	if cmd != "rddev review spawn "+taskID {
		t.Errorf("advice for a superseded verdict = %q (%s), want the fresh review", cmd, action)
	}
	if !strings.Contains(action, "older than the latest collect") {
		t.Errorf("the advice does not name the reason the gate gives: %q", action)
	}
}
