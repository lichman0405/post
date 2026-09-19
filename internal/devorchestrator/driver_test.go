package devorchestrator

import (
	"context"
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

// The driver's other exit. A loop that only ever acts cannot tell "the phase is
// done" from "I have not looked yet", and getting that wrong produces the exact
// failure this driver exists to remove: a process that stays alive holding the
// slot and the lock, looking healthy, doing nothing, forever.
//
// It had no test until the phase-boundary checkpoint went looking for one. That
// checkpoint's rule is that a property living in Go is checked by running the Go
// test, and there was none — so the property was being asserted by grepping for
// the message string. The grep proves the sentence exists; it does not prove the
// driver reaches it, and it says nothing about the state left behind.
func TestAnExhaustedDriverReportsCompletionInsteadOfWaiting(t *testing.T) {
	root := t.TempDir()
	dagPath := writeDAG(t, root)
	statePath := filepath.Join(root, "task_status.json")
	// Both tasks merged: nothing dispatchable, nothing running, no open
	// decision. The DAG agrees with the stub, so the stub's empty answer is the
	// real one and not a lie told to reach the branch.
	if err := os.WriteFile(statePath, []byte(`{"version":2,"tasks":{
		"T0001":{"status":"merged"},"T0002":{"status":"merged"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(root, "calls.log")
	var log strings.Builder

	o := &DriveOpts{
		RepoRoot: root, DagPath: dagPath, StatePath: statePath,
		Binary: writeIdleRddev(t, calls), Out: &log,
		StaleCheck: func() (string, bool) { return "", false },
		Poll:       10 * time.Millisecond,
		Parallel:   1,
	}
	// A deadline rather than a goroutine: an exhausted driver returns on its
	// first pass, so a correct build finishes in milliseconds while a broken one
	// fails here deterministically instead of hanging the suite or racing the
	// log buffer.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := o.Drive(ctx); err != nil {
		t.Fatalf("an exhausted driver kept waiting instead of reporting completion: %v", err)
	}

	st, err := ReadDriverStatus(root)
	if err != nil {
		t.Fatal(err)
	}
	// The state is the half that matters operationally: it is what `rddev
	// status` reads, and "idle" is the difference between a finished phase and
	// a driver that is merely quiet.
	if st == nil || st.State != "idle" {
		t.Errorf("completion is not on the record `rddev status` reads: %+v", st)
	}
	if !strings.Contains(log.String(), "nothing to do") {
		t.Errorf("the driver stopped without saying why:\n%s", log.String())
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

// A full Worker pool is a WAIT, not a judgement: a running Worker exits and
// frees a slot on its own, and there is nothing in that for the Supervisor to
// decide. Recorded as a decision it does not end the wait — the driver skips
// any task that has an open decision, and the decision carries the run id of a
// task that was never spawned (empty), which is exactly the kind the
// reconciler never clears. A momentary condition would therefore strand the
// task forever and silently, which is the one outcome §8.2 forbids. That was
// #163, and it stranded a task twice in one day.
//
// This is L1-20260913-17's rule — "a condition only the driver could fix is
// not a decision" — applied to the capacity gate, with the same division of
// labour as ciStillRunning (#139): one predicate decides what a refusal MEANS,
// and the call site decides what the driver DOES about it.
//
// All three spawn call sites fail through the same gate, so all three must
// answer the same way. A fix that only taught `dispatch` would leave both
// review-spawn paths able to strand a task the moment two Workers finish near
// enough together for one's review to find the pool full.
//
// The stub is rddev itself, so the call sites run in their real order and only
// the spawn fails.
func TestACapacityRefusalIsAWaitNotADecision(t *testing.T) {
	paths := []struct {
		name    string
		failing string
		status  string
		step    func(*DriveOpts) (bool, error)
	}{
		{
			"a Worker spawn",
			"worker spawn",
			"ready",
			func(o *DriveOpts) (bool, error) { return o.dispatch(&DriverStatus{}, nil), nil },
		},
		{
			"a review spawn",
			"review spawn",
			"verification",
			func(o *DriveOpts) (bool, error) { return o.stepVerification("T0001", &DriverStatus{}) },
		},
	}
	refusals := []struct {
		name     string
		text     string
		decision bool
	}{
		{
			"a full pool",
			parallelismLimit + ": 3 Worker(s) running, limit 3 (default 3, hard max 4) — retry after one finishes (rddev worker list)",
			false,
		},
		{
			"any other refusal",
			"allowed_scope of T0001 does not validate against the real tree: infra/migrations/** matches nothing",
			true,
		},
	}
	for _, p := range paths {
		for _, r := range refusals {
			t.Run(p.name+" refused for "+r.name, func(t *testing.T) {
				root := t.TempDir()
				dagPath := writeDAG(t, root)
				statePath := filepath.Join(root, "task_status.json")
				state := `{"version":2,"tasks":{"T0001":{"status":"` + p.status + `"}}}`
				if err := os.WriteFile(statePath, []byte(state), 0o644); err != nil {
					t.Fatal(err)
				}
				bin := writeRefusingRddev(t, p.failing, r.text)

				o := &DriveOpts{RepoRoot: root, DagPath: dagPath, StatePath: statePath, Binary: bin}
				acted, err := p.step(o)
				if err != nil {
					t.Fatal(err)
				}
				ds, err := ReadDecisions(root)
				if err != nil {
					t.Fatal(err)
				}
				if r.decision {
					if len(ds) != 1 {
						t.Fatalf("a real refusal must become exactly one decision for the Supervisor; got %d", len(ds))
					}
					if ds[0].Task != "T0001" {
						t.Errorf("the decision names %s, not T0001", ds[0].Task)
					}
					if !strings.Contains(ds[0].Reason, "allowed_scope") {
						t.Errorf("the decision does not carry the refusal:\n  %s", ds[0].Reason)
					}
					return
				}
				if acted {
					t.Error("a full Worker pool was treated as an action: the driver records a decision and stops retrying, so the task waits on the Supervisor for a slot that frees itself")
				}
				if len(ds) != 0 {
					t.Fatalf("a full Worker pool produced %d decision(s); it is a wait to retry next tick, not a judgement:\n  %s", len(ds), ds[0].Reason)
				}
			})
		}
	}
}

// writeRefusingRddev writes a fake rddev that answers every call with success
// except the named two-word subcommand, which fails with the refusal kept
// beside the stub, so the fixture is not also a shell-quoting exercise.
func writeRefusingRddev(t *testing.T, failing, refusal string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "rddev")
	script := "#!/bin/sh\n" +
		"case \"$1 $2\" in\n" +
		"  \"task next\") echo T0001;;\n" +
		"  \"" + failing + "\") cat \"$0.refusal\" >&2; exit 1;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin+".refusal", []byte(refusal+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return bin
}

// A driver whose own rddev has gone out of date cannot grade anything, because
// every command it runs hits the same guard. What it must NOT do is record that
// as a decision about each task: tick skips a task with an open decision, so the
// task stays wedged after the binary is rebuilt, and the fix (restart the
// driver) and the record (a landmine on T0001) are about different things.
//
// The failure this pins is not hypothetical — it happened twice on 2026-09-14
// alone, to T0304's push and to T0307's collect — so the assertion is on the two
// things that made it cost something: the driver ran no command at all, and it
// left nothing behind for a human to clear.
func TestAStaleDriverHoldsInsteadOfWedgingEveryTask(t *testing.T) {
	stale := "this rddev was built from aaaaaaaa, and main has since changed the orchestrator's own source:\n  bbbbbbbb orchestration: something"
	t.Setenv(AllowStaleBinaryEnv, "")
	root, dagPath, statePath := staleDriverFixture(t)
	calls := filepath.Join(root, "calls.log")
	var log strings.Builder

	o := &DriveOpts{
		RepoRoot: root, DagPath: dagPath, StatePath: statePath,
		Binary: writeRecordingRddev(t, calls), Out: &log,
		StaleCheck: func() (string, bool) { return stale, true },
	}
	st := &DriverStatus{}
	acted, err := o.tick(st)
	if err != nil {
		t.Fatal(err)
	}
	if acted {
		t.Error("a tick with a stale binary reported that it did something")
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Errorf("the driver ran a command while its own binary could not be trusted to grade one:\n  %s", readIfAny(t, calls))
	}
	ds, err := ReadDecisions(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 0 {
		t.Fatalf("a stale binary left %d decision(s) naming a task; the condition is about the driver process, and the task cannot clear it by being reworked:\n  %s", len(ds), ds[0].Reason)
	}
	if st.Stale != stale {
		t.Errorf("the heartbeat does not carry the staleness, so `rddev status` reports a driver holding as one that is working:\n  %q", st.Stale)
	}
	if !strings.Contains(log.String(), "stale rddev") {
		t.Errorf("the driver held without saying why:\n%s", log.String())
	}
}

// The other half: once the binary is current again — which in practice means the
// driver was restarted — the hold is gone by itself. Nothing has to be cleared,
// and the field does not stay set out of habit, or `rddev status` would report a
// rebuilt driver as stale forever.
func TestARebuiltDriverActsAgainWithoutAnythingBeingCleared(t *testing.T) {
	t.Setenv(AllowStaleBinaryEnv, "")
	root, dagPath, statePath := staleDriverFixture(t)
	calls := filepath.Join(root, "calls.log")
	var log strings.Builder

	o := &DriveOpts{
		RepoRoot: root, DagPath: dagPath, StatePath: statePath,
		Binary: writeRecordingRddev(t, calls), Out: &log,
		StaleCheck: func() (string, bool) { return "", false },
	}
	st := &DriverStatus{Stale: "this rddev was built from aaaaaaaa, and main has since changed"}
	if _, err := o.tick(st); err != nil {
		t.Fatal(err)
	}
	if st.Stale != "" {
		t.Errorf("a current binary still reports itself stale: %q", st.Stale)
	}
	if !strings.Contains(log.String(), "current rddev again") {
		t.Errorf("the hold was lifted without saying so, which is the same silence the hold exists to break:\n%s", log.String())
	}
	if _, err := os.Stat(calls); err != nil {
		t.Error("a current binary ran no command, so the hold is not what stopped the stale driver above")
	}
}

// RDDEV_ALLOW_STALE_BINARY means "run anyway". A driver started under it and
// then refusing to tick would turn the override into its opposite.
func TestTheStaleOverrideOutranksTheGuard(t *testing.T) {
	root, dagPath, statePath := staleDriverFixture(t)
	t.Setenv(AllowStaleBinaryEnv, "1")
	calls := filepath.Join(root, "calls.log")
	var log strings.Builder

	o := &DriveOpts{
		RepoRoot: root, DagPath: dagPath, StatePath: statePath,
		Binary: writeRecordingRddev(t, calls), Out: &log,
		StaleCheck: func() (string, bool) { return "stale, but the operator said to run anyway", true },
	}
	st := &DriverStatus{}
	if _, err := o.tick(st); err != nil {
		t.Fatal(err)
	}
	if st.Stale != "" {
		t.Errorf("a driver running under the override reported itself as holding: %q", st.Stale)
	}
	if _, err := os.Stat(calls); err != nil {
		t.Error("the override did not take effect: the driver ran no command")
	}
}

// staleDriverFixture is one running task whose Worker has exited — the state
// that makes tick reach for `worker collect` and therefore for the guard.
//
// It deliberately does not touch the override: whether the operator has said to
// run anyway is the test's subject, and a fixture that set it would decide the
// answer before the test could ask the question.
func staleDriverFixture(t *testing.T) (root, dagPath, statePath string) {
	t.Helper()
	root = t.TempDir()
	dagPath = writeDAG(t, root)
	statePath = filepath.Join(root, "task_status.json")
	if err := os.WriteFile(statePath, []byte(`{"version":2,"tasks":{"T0001":{"status":"running"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	exit := 0
	if err := SaveRegistry(root, &WorkerRecord{TaskID: "T0001", RunID: "run-1", PID: os.Getpid(), ExitStatus: &exit}); err != nil {
		t.Fatal(err)
	}
	if rec, err := LoadRegistry(root, "T0001"); err != nil || rec == nil || rec.ExitStatus == nil {
		t.Fatalf("the fixture did not survive the registry round trip: %v", err)
	}
	return root, dagPath, statePath
}

// writeRecordingRddev writes a fake rddev that appends every invocation to
// calls and succeeds, so "the driver acted" and "the driver held" are told
// apart by a file rather than by reading the code.
func writeRecordingRddev(t *testing.T, calls string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "rddev")
	script := "#!/bin/sh\n" +
		"echo \"$1 $2\" >> \"" + calls + "\"\n" +
		"case \"$1 $2\" in\n" +
		"  \"task next\") echo T0001;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// writeIdleRddev writes a fake rddev that reports no dispatchable task. The
// difference from writeRecordingRddev is the whole point of the test above:
// "T0001" is work to do, and empty is the end of the phase.
func writeIdleRddev(t *testing.T, calls string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "rddev")
	script := "#!/bin/sh\n" +
		"echo \"$1 $2\" >> \"" + calls + "\"\n" +
		"exit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func readIfAny(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return "(unreadable)"
	}
	return strings.TrimSpace(string(b))
}

// A heartbeat that is only written between ticks goes stale during the one step
// that blocks longest: acceptance shells out to `rddev task accept`, which
// re-runs the gate suite (~9 minutes, three times StaleAfter). The window is a
// claim about the process — silence means it is gone, never that it is busy —
// so it has to hold while the driver is inside a tick, which is the state a
// reader finds it in most of the time it is doing something expensive.
func TestTheHeartbeatStaysFreshWhileATickIsBlocked(t *testing.T) {
	root := t.TempDir()
	const stale = "1970-01-01T00:00:00.000Z"
	if err := WriteDriverStatus(root, &DriverStatus{PID: os.Getpid(), HeartbeatAt: stale, State: "starting"}); err != nil {
		t.Fatal(err)
	}

	// The main loop is "blocked in a tick" here: nothing else writes the file
	// while the keepalive runs.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := startHeartbeat(ctx, root, os.Getpid(), 20*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := ReadDriverStatus(root)
		if err != nil {
			t.Fatal(err)
		}
		if got.HeartbeatAt != stale {
			if !got.Alive(time.Now()) {
				t.Fatalf("the keepalive wrote a heartbeat that reads as dead: %q", got.HeartbeatAt)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the heartbeat was never refreshed while the tick was blocked: " +
				"a driver in the middle of an acceptance reads as dead")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Stopping it stops the writes: a keepalive that outlives its driver would
	// keep a crashed process looking alive forever.
	stop()
	before, err := ReadDriverStatus(root)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	after, err := ReadDriverStatus(root)
	if err != nil {
		t.Fatal(err)
	}
	if after.HeartbeatAt != before.HeartbeatAt {
		t.Errorf("the heartbeat kept moving after the keepalive was stopped: %q then %q",
			before.HeartbeatAt, after.HeartbeatAt)
	}

	// And it leaves someone else's status file alone: a driver that died left
	// its file behind, and the adoption that follows must not be papered over
	// by a keepalive refreshing a heartbeat it does not own.
	if err := WriteDriverStatus(root, &DriverStatus{PID: os.Getpid() + 1, HeartbeatAt: stale}); err != nil {
		t.Fatal(err)
	}
	stop2 := startHeartbeat(context.Background(), root, os.Getpid(), 20*time.Millisecond)
	defer stop2()
	time.Sleep(100 * time.Millisecond)
	other, err := ReadDriverStatus(root)
	if err != nil {
		t.Fatal(err)
	}
	if other.HeartbeatAt != stale {
		t.Errorf("the keepalive refreshed a status file belonging to pid %d: %q", os.Getpid()+1, other.HeartbeatAt)
	}
}
