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

// The driver's files live under .rddev/runtime/, which the Worker guard blocks
// for reads and writes — so a Worker cannot plant a symlink there today. This is
// not about today: the driver runs as the Supervisor, and an open that follows a
// link would truncate or disclose whatever it pointed at with the Supervisor's
// reach. Verified both ways, because a refusal that never fires is not a guard.
func TestTheDriverRefusesToFollowASymlink(t *testing.T) {
	root := t.TempDir()
	paths := DriverFilesAt(root)
	if err := os.MkdirAll(filepath.Dir(paths.Status), 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim.json")
	if err := os.WriteFile(victim, []byte(`{"pid":1,"state":"planted"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// The lock: truncating through a link would destroy the linked file.
	if err := os.Symlink(victim, paths.Lock); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	if _, err := AcquireDriverLock(root); err == nil {
		t.Fatal("the driver lock followed a symlink — it would truncate whatever the link pointed at")
	}
	if b, _ := os.ReadFile(victim); !strings.Contains(string(b), "planted") {
		t.Fatal("the linked file was modified by the refused lock open")
	}

	// The status and decisions reads: following one pulls an arbitrary file
	// into the Supervisor's view.
	for _, p := range []string{paths.Status, paths.Decisions} {
		_ = os.Remove(p)
		if err := os.Symlink(victim, p); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadDriverStatus(root); err == nil && p == paths.Status {
			t.Errorf("%s was read through a symlink", p)
		}
		if _, err := ReadDecisions(root); err == nil && p == paths.Decisions {
			t.Errorf("%s was read through a symlink", p)
		}
	}

	// And a real file in the same place still works, so the guard is not just
	// refusing everything.
	_ = os.Remove(paths.Status)
	if err := WriteDriverStatus(root, &DriverStatus{PID: 7, HeartbeatAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	if st, err := ReadDriverStatus(root); err != nil || st == nil || st.PID != 7 {
		t.Fatalf("a genuine status file was refused: %v %+v", err, st)
	}
}
