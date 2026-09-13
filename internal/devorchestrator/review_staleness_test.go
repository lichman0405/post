package devorchestrator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeReviewGate records a Review Worker gate-inputs document carrying the
// given reviewed-code fingerprint — the shape SpawnReview writes, written
// directly here because these tests are about the comparison, not the spawn.
func writeReviewGate(t *testing.T, repoRoot, taskID, fingerprint string) {
	t.Helper()
	reviewID := ReviewTaskID(taskID)
	if err := os.MkdirAll(RuntimeTasksDir(repoRoot, reviewID), 0o755); err != nil {
		t.Fatal(err)
	}
	gate := &GateInputs{TaskID: reviewID, RunID: "run-review", SessionID: "session-review", ReviewDiffSHA: fingerprint}
	if err := writeFileAtomic(gateInputsPath(repoRoot, reviewID), marshalIndentBytes(gate)); err != nil {
		t.Fatal(err)
	}
}

// ReviewIsStale is the driver's copy of the comparison review collect makes.
// These tests pin that it answers the same question the gate asks: a verdict
// the gate would refuse must be one the driver re-dispatches, and a verdict the
// gate would accept must not cost a fresh review. The driver used to record a
// decision for the first case instead, which turned every rework into a task
// waiting for a human to say "review it again" (L1-20260913-17).
func TestReviewIsStaleIsTheGatesComparison(t *testing.T) {
	root := t.TempDir()
	gitIn := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	gitIn("init", "-q", "-b", DefaultBaseBranch)
	if err := os.WriteFile(filepath.Join(root, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn("add", "-A")
	gitIn("commit", "-q", "-m", "base")
	head := gitIn("rev-parse", "HEAD")

	// The Worker's deliverable, in the shape it has while the Worker works: an
	// uncommitted file. codeIdentity hashes content, so this is what a verdict
	// is bound to.
	//
	// The worktree is nested inside the repo like the real one
	// (.rddev/worktrees/<TASK>): a fixture that used the repo root as the
	// worktree would put the runtime dir INSIDE it, and the review record this
	// test writes would then read as a changed file — the fixture failing the
	// test instead of the code.
	wt := filepath.Join(root, ".rddev", "worktrees", "T0100")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(wt, "work.txt")
	if err := os.WriteFile(work, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	taskID := "T0100"
	rec := &WorkerRecord{
		TaskID: taskID, RunID: "run-task", SessionID: "session-task",
		BaselineSHA: head, Branch: "task/T0100-x",
		Worktree: wt, ResultDir: filepath.Join(root, ".rddev", "workers", taskID),
		LogPath: filepath.Join(root, "worker.log"), StartedAt: "t",
	}
	if err := SaveRegistry(root, rec); err != nil {
		t.Fatal(err)
	}
	fp, err := codeIdentity(rec)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture must be inert with respect to the thing under test: if the
	// identity moved on its own, every assertion below would be about the
	// fixture's churn rather than the code's condition.
	if again, err := codeIdentity(rec); err != nil || again != fp {
		t.Fatalf("codeIdentity is not stable on an unchanged tree (%s then %s, err=%v) — the fixture is measuring its own writes", fp, again, err)
	}

	// No review record yet: not stale. "Is there a verdict at all" is the
	// collect's question, and answering it here would respawn reviews the gate
	// was about to accept.
	if stale, why, err := ReviewIsStale(root, taskID); err != nil || stale {
		t.Fatalf("no review record yet: stale=%v why=%q err=%v, want false", stale, why, err)
	}

	// A review spawned against this code: current, and must not be respawned —
	// a fresh Review Worker costs a session, so a false positive is a real cost.
	writeReviewGate(t, root, taskID, fp)
	if stale, why, err := ReviewIsStale(root, taskID); err != nil || stale {
		t.Fatalf("a review of the current code was judged superseded: stale=%v why=%q err=%v", stale, why, err)
	}

	// The rework changes the code. The recorded verdict is now about a state
	// that no longer exists: collect refuses it (correctly) and would refuse it
	// forever, because the Reviewer has exited and its verdict cannot change.
	if err := os.WriteFile(work, []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale, why, err := ReviewIsStale(root, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if !stale {
		t.Fatalf("a verdict about a superseded attempt was judged current — the driver would collect it and fail on every tick; identity was %s", fp)
	}
	if !strings.Contains(why, "superseded") {
		t.Errorf("the reason does not say what is wrong, so a log line cannot explain the respawn: %q", why)
	}

	// A review record with no fingerprint (a pre-fingerprint spawn) is not
	// judged stale: collect decides that case, and it refuses an unanchored
	// review outright.
	writeReviewGate(t, root, taskID, "")
	if stale, why, err := ReviewIsStale(root, taskID); err != nil || stale {
		t.Fatalf("a review record without a fingerprint: stale=%v why=%q err=%v, want false (the gate owns that judgement)", stale, why, err)
	}
}
