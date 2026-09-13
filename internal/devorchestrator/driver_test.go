package devorchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Two drivers would double-collect and double-merge. The slot is a file lock,
// so the second refuses immediately and names the holder rather than queueing
// behind it.
func TestOnlyOneDriverAtATime(t *testing.T) {
	root := t.TempDir()
	first, err := AcquireDriverLock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()

	if _, err := AcquireDriverLock(root); err == nil {
		t.Fatal("a second driver acquired the slot while the first held it")
	} else if !strings.Contains(err.Error(), "another driver already holds the slot") {
		t.Errorf("the refusal does not say what happened: %v", err)
	}

	// And the slot is released when the holder lets go, so a crashed driver
	// does not wedge the repository forever (the OS drops the lock with the
	// process).
	first.Release()
	again, err := AcquireDriverLock(root)
	if err != nil {
		t.Fatalf("the slot did not free after release: %v", err)
	}
	again.Release()
}

// "Alive" is a fact read from the heartbeat, not an inference from a process
// table: a crashed driver must be reported dead rather than merely unreachable,
// and a driver blocked on a long Worker must not be mistaken for one.
func TestDriverAliveIsDecidedByTheHeartbeat(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()

	if st, err := ReadDriverStatus(root); err != nil || st != nil {
		t.Fatalf("a repository with no driver should report no status: %v %v", st, err)
	}

	fresh := &DriverStatus{PID: 1, HeartbeatAt: now.Add(-10 * time.Second).Format(time.RFC3339)}
	if err := WriteDriverStatus(root, fresh); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDriverStatus(root)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Alive(now) {
		t.Error("a heartbeat 10s old was reported dead")
	}
	// A driver waiting on a 90-minute Worker still heartbeats every tick, so
	// silence past the window means the process is gone, not busy.
	if got.Alive(now.Add(StaleAfter + time.Second)) {
		t.Error("a heartbeat older than the window was reported alive — a crashed driver would look healthy")
	}
}

// A refusal is a judgement call, and the judgement has to survive the turn that
// would make it. A decision is cleared by the act that answers it — a rework or
// respawn changes the run it was about — not by asking twice.
func TestDecisionsSurviveAndClearThemselves(t *testing.T) {
	root := t.TempDir()
	if err := RecordDecision(root, Decision{Task: "T0201", Action: "accept", Reason: "gate refused", RunID: "run-old"}); err != nil {
		t.Fatal(err)
	}
	ds, err := ReadDecisions(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || ds[0].Task != "T0201" {
		t.Fatalf("decision not recorded: %+v", ds)
	}

	// The task is still on the run the decision was about: the decision stands.
	if err := SaveRegistry(root, &WorkerRecord{
		TaskID: "T0201", RunID: "run-old", SessionID: "s", ClaudeVersion: "v",
		PID: 1, StartTime: 1, Worktree: root, Branch: "b", BaselineSHA: "x",
		RefsBefore: []string{}, LogPath: filepath.Join(root, "l"), ResultDir: root, StartedAt: "t",
	}); err != nil {
		t.Fatal(err)
	}
	if err := staleDecisions(root); err != nil {
		t.Fatal(err)
	}
	if ds, _ = ReadDecisions(root); len(ds) != 1 {
		t.Fatalf("the decision was cleared while its run was still current: %+v", ds)
	}

	// The task was reworked: the attempt the decision was about is gone, so the
	// decision is answered by that act and the driver resumes on its own.
	if err := SaveRegistry(root, &WorkerRecord{
		TaskID: "T0201", RunID: "run-new", SessionID: "s", ClaudeVersion: "v",
		PID: 1, StartTime: 1, Worktree: root, Branch: "b", BaselineSHA: "x",
		RefsBefore: []string{}, LogPath: filepath.Join(root, "l"), ResultDir: root, StartedAt: "t",
	}); err != nil {
		t.Fatal(err)
	}
	if err := staleDecisions(root); err != nil {
		t.Fatal(err)
	}
	if ds, _ = ReadDecisions(root); len(ds) != 0 {
		t.Fatalf("a decision outlived the run it was about: %+v", ds)
	}
	_ = os.Remove(filepath.Join(root, ".rddev", "runtime", "decisions.json"))
}
