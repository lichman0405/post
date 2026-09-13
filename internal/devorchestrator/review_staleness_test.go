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

// writeReviewRegistry records a dispatched, exited Review Worker for the task —
// the state the driver acts on. ReviewIsStale is asked about a review that
// exists (the driver spawns when it does not), and a fixture without this
// record would be asking about a review nobody dispatched.
func writeReviewRegistry(t *testing.T, repoRoot, taskID string) {
	t.Helper()
	reviewID := ReviewTaskID(taskID)
	if err := os.MkdirAll(RuntimeTasksDir(repoRoot, reviewID), 0o755); err != nil {
		t.Fatal(err)
	}
	exited := 0
	if err := SaveRegistry(repoRoot, &WorkerRecord{
		TaskID: reviewID, RunID: "run-review", SessionID: "session-review",
		ResultDir: WorkerTaskDir(repoRoot, reviewID),
		LogPath:   filepath.Join(WorkerTaskDir(repoRoot, reviewID), "worker.log"),
		StartedAt: nowRFC3339(), ExitStatus: &exited,
	}); err != nil {
		t.Fatal(err)
	}
}

// taskFixture builds the smallest repository the review lifecycle is decided
// from: a git repo, a task worktree holding one uncommitted deliverable, the
// task's registry record, and the code identity of that state.
//
// The worktree is nested inside the repo like the real one
// (.rddev/worktrees/<TASK>): a fixture that used the repo root as the worktree
// would put the runtime record inside it, and the fixture's own writes would
// read as a changed file — the fixture failing the test instead of the code.
func taskFixture(t *testing.T, taskID string) (root string, rec *WorkerRecord, work string, fp string) {
	t.Helper()
	root = t.TempDir()
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

	wt := filepath.Join(root, ".rddev", "worktrees", taskID)
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	// The Worker's deliverable, in the shape it has while the Worker works: an
	// uncommitted file. codeIdentity hashes content, so this is what a verdict
	// is bound to.
	work = filepath.Join(wt, "work.txt")
	if err := os.WriteFile(work, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec = &WorkerRecord{
		TaskID: taskID, RunID: "run-task", SessionID: "session-task",
		BaselineSHA: gitIn("rev-parse", "HEAD"), Branch: "task/" + taskID + "-x",
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
	return root, rec, work, fp
}

// ReviewIsStale is the driver's copy of the comparison review collect makes.
// These tests pin that it answers the same question the gate asks: a verdict
// the gate would refuse must be one the driver re-dispatches, and a verdict the
// gate would accept must not cost a fresh review. The driver used to record a
// decision for the first case instead, which turned every rework into a task
// waiting for a human to say "review it again" (L1-20260913-17).
func TestReviewIsStaleIsTheGatesComparison(t *testing.T) {
	taskID := "T0100"
	root, _, work, fp := taskFixture(t, taskID)

	// No review record yet: not stale. "Is there a verdict at all" is the
	// collect's question, and answering it here would respawn reviews the gate
	// was about to accept.
	if stale, why, err := ReviewIsStale(root, taskID); err != nil || stale {
		t.Fatalf("no review record yet: stale=%v why=%q err=%v, want false", stale, why, err)
	}

	// A review spawned against this code: current, and must not be respawned —
	// a fresh Review Worker costs a session, so a false positive is a real cost.
	writeReviewGate(t, root, taskID, fp)
	writeReviewRegistry(t, root, taskID)
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

	// A review record with no fingerprint (a pre-fingerprint spawn) is stale
	// too, and for the same reason as the case above: no reviewer will ever add
	// a fingerprint to a record that already exists, so collect refuses the
	// verdict permanently and only a fresh review can move the task. Leaving it
	// to collect would be the L1-20260913-17 stall again, one step later.
	writeReviewGate(t, root, taskID, "")
	if stale, why, err := ReviewIsStale(root, taskID); err != nil || !stale {
		t.Fatalf("a review record without a fingerprint: stale=%v why=%q err=%v, want true — collect refuses it forever", stale, why, err)
	} else if !strings.Contains(why, "fingerprint") {
		t.Errorf("the reason does not say what is missing: %q", why)
	}

	// No authoritative record at all (a pre-T0012 spawn): same answer, same
	// reason. collect's review-gate-inputs check refuses it outright.
	if err := os.Remove(gateInputsPath(root, ReviewTaskID(taskID))); err != nil {
		t.Fatal(err)
	}
	if stale, why, err := ReviewIsStale(root, taskID); err != nil || !stale {
		t.Fatalf("a review with no authoritative spawn record: stale=%v why=%q err=%v, want true", stale, why, err)
	} else if !strings.Contains(why, "spawn record") {
		t.Errorf("the reason does not name the missing record: %q", why)
	}
}

// The driver's own decision has to be the respawn, not a recorded decision for
// the Supervisor: a rework after a refusal leaves the verdict describing code
// that no longer exists, the reviewer will never run again, and so every tick
// would refuse the same collect and re-record the same decision — a pipeline
// waiting on a human to say "review it again" (L1-20260913-17).
//
// It drives stepVerification itself, with rddev replaced by a recorder, because
// the helper test above cannot see a regression at the call site: deleting the
// staleness branch from the driver would leave every assertion in it green.
func TestTheDriverRespawnsASupersededReviewInsteadOfRecordingADecision(t *testing.T) {
	root, rec, work, fp := taskFixture(t, "T0100")
	writeReviewGate(t, root, "T0100", fp)
	writeReviewRegistry(t, root, "T0100")

	// rddev itself, replaced by a recorder: the driver decides WHAT to attempt,
	// and this test is about that decision.
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(root, "calls.log")
	stub := filepath.Join(bin, "rddev-stub")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + calls + "\nexit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	o := &DriveOpts{RepoRoot: root, Binary: stub, Out: &log}
	st := &DriverStatus{}

	// Control: the reviewed code is current, so the driver must NOT spend a
	// session on a fresh review — it collects and then accepts.
	if _, err := o.stepVerification("T0100", st); err != nil {
		t.Fatal(err)
	}
	if got := readFileString(t, calls); !strings.Contains(got, "review collect T0100") || strings.Contains(got, "review spawn T0100") {
		t.Fatalf("a current review: rddev calls %q, want collect and no respawn", got)
	}
	if err := os.Remove(calls); err != nil {
		t.Fatal(err)
	}

	// The rework: same task, different code.
	if err := os.WriteFile(work, []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	acted, err := o.stepVerification("T0100", st)
	if err != nil {
		t.Fatalf("the driver returned an error instead of answering: %v", err)
	}
	if !acted {
		t.Error("the driver reported that it did nothing while a review needed re-dispatching — the tick would idle")
	}
	got := readFileString(t, calls)
	if !strings.Contains(got, "review spawn T0100") {
		t.Fatalf("a superseded review was not respawned: rddev calls %q", got)
	}
	if strings.Contains(got, "task accept T0100") {
		t.Errorf("the driver accepted the task on a verdict about superseded code: %q", got)
	}
	if ds, err := ReadDecisions(root); err != nil {
		t.Fatal(err)
	} else if len(ds) != 0 {
		t.Errorf("the driver recorded a decision (%+v) for something it can fix itself — that is the stall L1-20260913-17 was", ds)
	}
	if !strings.Contains(log.String(), "superseded") {
		t.Errorf("the log does not say why a fresh review was dispatched: %q", log.String())
	}

	// Failing to ANSWER the question must not stop the driver either: Drive
	// propagates a step error and the long-lived process exits, so one
	// unreadable worktree would take the whole pipeline down while it still
	// looked alive. The judgement falls to collect, which asks the same
	// question of the same facts and refuses on its own terms.
	if err := os.Remove(calls); err != nil {
		t.Fatal(err)
	}
	rec.Worktree = t.TempDir() // a directory that is not a git work tree
	if err := SaveRegistry(root, rec); err != nil {
		t.Fatal(err)
	}
	log.Reset()
	acted, err = o.stepVerification("T0100", st)
	if err != nil {
		t.Fatalf("the driver returned an error for a question it could not answer — Drive would exit and the pipeline would stop: %v", err)
	}
	if !acted {
		t.Error("the driver reported that it did nothing")
	}
	if got := readFileString(t, calls); !strings.Contains(got, "review collect T0100") {
		t.Errorf("the driver did not fall through to collect, which owns the judgement: rddev calls %q", got)
	}
	if !strings.Contains(log.String(), "cannot tell whether the review is superseded") {
		t.Errorf("the log does not say the judgement was left to collect: %q", log.String())
	}
}

// `rddev workflow` is how an interrupted session resumes from disk alone, so
// its "next action" has to agree with the gate that will judge it. An approve
// verdict about superseded code is refused by the merge gate by name, and
// advice that walks the Supervisor into that refusal — "run the acceptance
// gate" — costs a round trip and reads like a failure of the work rather than
// of the verdict.
func TestTheWorkflowAdviceForASupersededVerdictIsAReReview(t *testing.T) {
	taskID := "T0100"
	root, _, work, fp := taskFixture(t, taskID)

	approved := func(diffSHA string) *WorkflowView {
		return &WorkflowView{Review: &ReviewRecord{Verdict: "approve", DiffSHA: diffSHA}}
	}

	// The verdict describes the code that is there: the acceptance gate is the
	// next step, and nothing may be re-reviewed.
	if action, cmd := nextActionFor(root, taskID, StateVerification, approved(fp)); cmd != "rddev task accept "+taskID {
		t.Errorf("a verdict about the current code: advice %q / %q, want the acceptance gate", action, cmd)
	}

	// The code moved after the verdict: accept would run the acceptance gate on
	// a verdict the merge gate refuses.
	if err := os.WriteFile(work, []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	action, cmd := nextActionFor(root, taskID, StateVerification, approved(fp))
	if cmd != "rddev review spawn "+taskID {
		t.Errorf("a verdict about superseded code: advice %q / %q, want a fresh review", action, cmd)
	}
	if !strings.Contains(action, "superseded") {
		t.Errorf("the advice does not say what is wrong with the verdict: %q", action)
	}

	// A verdict with no identity at all is refused by the merge gate for the
	// same reason, one branch earlier there.
	if _, cmd := nextActionFor(root, taskID, StateVerification, approved("")); cmd != "rddev review spawn "+taskID {
		t.Errorf("a verdict with no code identity: advice %q, want a fresh review", cmd)
	}

	// A request_changes verdict is a rejection, not a re-review: the existing
	// advice stands, and this test does not blur the two.
	changes := &WorkflowView{Review: &ReviewRecord{Verdict: "request_changes", DiffSHA: fp}}
	if _, cmd := nextActionFor(root, taskID, StateVerification, changes); !strings.Contains(cmd, "task reject") {
		t.Errorf("a request_changes verdict: advice %q, want the rejection path", cmd)
	}
}

// readFileString reads a file the fixture wrote; a missing one is empty.
func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
