package devorchestrator

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startGroupWithGrandchild starts a session/group leader (setsid, exactly
// like spawn does) that leaves a background child behind and reports that
// child's pid. The leader is its own group's id, so it is the negative-pid
// target; the grandchild is what a positive-pid signal would miss.
func startGroupWithGrandchild(t *testing.T) (leader *os.Process, grandchild int) {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	// The background child is the #166 shape: started by the Worker's own
	// shell, in the Worker's group, outliving the command that started it.
	//
	// The leading exec detaches the whole subtree from the test's stdout: a
	// leaked leftover holding the pipe open makes `go test` wait for EOF on it
	// long after the tests are done (learned the hard way — the negative
	// control for this very test hung the suite for the leftover's full 300s).
	cmd := startSetsidChild(t, "bash", "-c", "exec >/dev/null 2>&1; sleep 300 & echo $! > "+pidFile+"; sleep 300")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				return cmd.Process, pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the group leader never reported its child pid")
	return nil, 0
}

// TestSignalProcessGroupReachesTheWorkersChildren is #166's core: a positive
// pid signals exactly one process, so everything a Worker started outlives
// it. rddev gives every Worker its own group on purpose (setsid, then job
// control in the reaper) — the group is the unit it means.
func TestSignalProcessGroupReachesTheWorkersChildren(t *testing.T) {
	leader, grandchild := startGroupWithGrandchild(t)
	leaderPID := leader.Pid

	if got := processGroupOf(leaderPID); got != leaderPID {
		t.Fatalf("processGroupOf(leader %d) = %d, want %d — a setsid child leads its own group", leaderPID, got, leaderPID)
	}
	if got := processGroupOf(grandchild); got != 0 {
		t.Errorf("processGroupOf(%d) = %d, want 0 — the grandchild is a group MEMBER; using its pid as a group target would miss or misdirect", grandchild, got)
	}

	if err := signalProcessGroup(leaderPID, syscall.SIGTERM); err != nil {
		t.Fatalf("signalProcessGroup: %v", err)
	}

	if !waitProcessNotRunning(leaderPID, 5*time.Second) {
		t.Errorf("the group leader %d survived SIGTERM to its group", leaderPID)
	}
	if !waitProcessNotRunning(grandchild, 5*time.Second) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL) // do not leak the evidence
		t.Errorf("pid %d, started by the Worker, survived SIGTERM to the Worker's group: signalling the positive pid would leave exactly this", grandchild)
	}
}

// TestSignalProcessGroupFallsBackToTheProcess: a pid that is not a group
// leader must still be signalled (as a plain process). Skipping it would turn
// "stop this Worker" into a silent no-op for any record written before the
// group existed.
func TestSignalProcessGroupFallsBackToTheProcess(t *testing.T) {
	_, grandchild := startGroupWithGrandchild(t)
	if err := signalProcessGroup(grandchild, syscall.SIGKILL); err != nil {
		t.Fatalf("signalProcessGroup on a non-leader pid: %v", err)
	}
	if !waitProcessNotRunning(grandchild, 5*time.Second) {
		t.Errorf("pid %d was not signalled — a non-leader pid must fall back to a plain signal", grandchild)
	}
}

// TestResidueReportTellsTheSupervisorWhatToDo is #166's second half: the Gate
// is right to refuse, but it must say which of the two very different
// situations it found and, when the owner is gone, what to run.
func TestResidueReportTellsTheSupervisorWhatToDo(t *testing.T) {
	_, grandchild := startGroupWithGrandchild(t)
	findings := []ProcessFinding{{PID: grandchild, Cmdline: "sleep 300", Session: grandchild, PGID: grandchild}}

	// (1) The owner has already exited: these are leftovers, and the report
	// must carry the command that collects them.
	gone := &WorkerRecord{TaskID: "T0206", PID: 999999, StartTime: 0}
	out := residueReport("Reviewer", gone, findings)
	for _, want := range []string{"has already exited", "still running: pid", "session", "group", "kill -TERM -- -"} {
		if !strings.Contains(out, want) {
			t.Errorf("the leftover report is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "rddev worker stop") {
		t.Errorf("a dead owner must not be sent to a stop command:\n%s", out)
	}

	// (2) The owner is still running: these may be work in progress, and the
	// report must say so instead of inviting a kill.
	alivePID := syscall.Getpid()
	ticks, err := procStartTicks(alivePID)
	if err != nil {
		t.Fatal(err)
	}
	alive := &WorkerRecord{TaskID: "T0206", PID: alivePID, StartTime: ticks}
	out = residueReport("Worker", alive, findings)
	if !strings.Contains(out, "still running") || !strings.Contains(out, "rddev worker stop T0206") {
		t.Errorf("a live owner must be told to stop it first:\n%s", out)
	}
	if strings.Contains(out, "kill -TERM") {
		t.Errorf("a live owner's processes must not be offered up for killing:\n%s", out)
	}

	// (3) A group that holds this very process must never be printed as a
	// kill target — the Supervisor would take out the shell running it.
	mine := []ProcessFinding{{PID: 999999, Cmdline: "self", Session: 0, PGID: syscall.Getpgrp()}}
	if out := residueReport("Worker", gone, mine); strings.Contains(out, "kill -TERM") {
		t.Errorf("the report offers to kill rddev's own process group:\n%s", out)
	}
}
