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

// Two defects in the first version of the loop, each of which left the driver
// alive, heartbeating and doing nothing. Both are pinned here rather than left
// to the e2e, which exercised neither.
func TestTheDriverInvokesRddevWithFlagsAfterTheSubcommand(t *testing.T) {
	o := &DriveOpts{DagPath: "tasks/tasks.json", StatePath: "tasks/task_status.json"}
	got := o.rddevArgs([]string{"worker", "collect", "T0201"})
	want := []string{"worker", "collect", "T0201", "--tasks-json", "tasks/tasks.json", "--state-json", "tasks/task_status.json"}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("args = %v, want %v — --tasks-json is a PER-COMMAND flag: before the subcommand it IS read as the subcommand, and every action fails with a usage error", got, want)
		}
	}
}

// Collecting and then stopping — because verification and accepted were never
// revisited — is a pipeline that ends at the first gate.
func TestTheLoopRevisitsVerificationAndAccepted(t *testing.T) {
	root := t.TempDir()
	dagPath, statePath := writeDAG(t, root), filepath.Join(root, "task_status.json")
	if err := os.WriteFile(statePath, []byte(`{"version":2,"tasks":{
		"T0001":{"status":"running"},"T0002":{"status":"verification"},"T0003":{"status":"todo"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// writeDAG's fixture has T0001 and T0002; add a third so accepted is covered.
	o := &DriveOpts{RepoRoot: root, DagPath: dagPath, StatePath: statePath}
	pending, err := o.tasksNeedingAction()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, id := range pending {
		seen[id] = true
	}
	if !seen["T0001"] {
		t.Error("a running task is not revisited: its Worker's exit would never be noticed")
	}
	if !seen["T0002"] {
		t.Error("a task in verification is not revisited: it would be collected and then never reviewed, accepted, merged or dispatched to CI")
	}
	if seen["T0003"] {
		t.Error("a todo task was treated as needing action — that is what the DAG's dependencies are for")
	}
}

// The classification in git_control.go is only half of issue #139. It decides
// which sentence a refusal carries; stepAccepted decides what the sentence
// MEANS for the pipeline, and until this test nothing exercised that half:
// replacing `if ciStillRunning(out)` with an unconditional retry left every
// test in the package green, while in production it would have put the driver
// straight back into retrying a finished red check on every tick forever.
//
// The stub is rddev itself, so the whole of stepAccepted runs — commit, push,
// open, status, merge — in the order it really does, and only the merge fails.
func TestARedMergeRefusalBecomesADecisionAndAWaitDoesNot(t *testing.T) {
	cases := []struct {
		name     string
		refusal  string
		decision bool
	}{
		{
			"a required check that finished red",
			checksRed + " — not passing: go (FAILURE) — the local G2 record is a replica of CI, not CI itself",
			true,
		},
		{
			"a required check still running",
			checksNotYet + " — still running: go (IN_PROGRESS) — the local G2 record is a replica of CI, not CI itself",
			false,
		},
		{
			"a PR whose checks have not registered yet",
			noChecksReported + " on the 'task/T0001-x' branch",
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dagPath, statePath := writeDAG(t, root), filepath.Join(root, "task_status.json")
			if err := os.WriteFile(statePath, []byte(`{"version":2,"tasks":{"T0001":{"status":"accepted"}}}`), 0o644); err != nil {
				t.Fatal(err)
			}
			// The refusal travels beside the stub rather than inside its text,
			// so the fixture is not also a shell-quoting exercise.
			bin := filepath.Join(t.TempDir(), "rddev")
			script := "#!/bin/sh\n" +
				"case \"$1 $2\" in\n" +
				"  \"pr merge\") cat \"$0.refusal\" >&2; exit 1;;\n" +
				"esac\nexit 0\n"
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bin+".refusal", []byte(tc.refusal+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			o := &DriveOpts{RepoRoot: root, DagPath: dagPath, StatePath: statePath, Binary: bin}
			st := &DriverStatus{}
			acted, err := o.stepAccepted("T0001", st)
			if err != nil {
				t.Fatal(err)
			}
			ds, err := ReadDecisions(root)
			if err != nil {
				t.Fatal(err)
			}
			if tc.decision {
				if !acted {
					t.Error("a red check produced no action, so the next tick would run the merge again — the loop of #139")
				}
				if len(ds) != 1 {
					t.Fatalf("a finished red check must become exactly one decision for the Supervisor; got %d", len(ds))
				}
				if ds[0].Task != "T0001" || ds[0].Action != "merge" {
					t.Errorf("the decision names %s/%s, not the task's merge", ds[0].Task, ds[0].Action)
				}
				if !strings.Contains(ds[0].Reason, "FAILURE") {
					t.Errorf("the decision does not say which check failed:\n  %s", ds[0].Reason)
				}
				if st.Merged != 0 {
					t.Errorf("a refused merge was counted as merged (%d)", st.Merged)
				}
				return
			}
			if acted {
				t.Error("a still-running check was treated as an action: the driver records a decision and stops retrying, so the merge the CI would have allowed never happens")
			}
			if len(ds) != 0 {
				t.Fatalf("a still-running check produced %d decisions; it is a wait to retry next tick, not a judgement:\n  %s", len(ds), ds[0].Reason)
			}
			if st.Merged != 0 {
				t.Errorf("nothing merged, but Merged=%d", st.Merged)
			}
		})
	}
}

// A derived artifact is a FUNCTION of its inputs, so merging it as text is
// meaningless: the branch's copy restores a digest describing the branch's old
// specs, and main's drops the task's spec edits. Rebaseline regenerates them
// instead — which requires knowing how. A new derived artifact without a
// regenerator would make the advance refuse at the moment it is needed, so the
// gap is asserted here, where it is cheap.
func TestEveryDerivedArtifactHasARegenerator(t *testing.T) {
	root := repoRootOf(t)
	derived, err := LoadDerivedArtifacts(filepath.Join(root, DefaultDerivedArtifactsPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range derived.Rules {
		gen, ok := regenerators[r.Derived]
		if !ok {
			t.Errorf("%s is declared derived from %s with no regenerator — rebaseline would refuse to advance any task that touches it. Add one to regenerators in rebaseline.go", r.Derived, r.Marker)
			continue
		}
		// And both commands must exist, or the refusal happens at the worst
		// time — the write during an advance, the check right after it.
		for _, cmd := range [][]string{gen.Write, gen.Check} {
			if len(cmd) < 2 {
				t.Errorf("%s has a malformed regenerator %v", r.Derived, cmd)
				continue
			}
			if _, err := os.Stat(filepath.Join(root, cmd[1])); err != nil {
				t.Errorf("the regenerator for %s runs %q, which does not exist: %v", r.Derived, cmd[1], err)
			}
		}
	}
}
