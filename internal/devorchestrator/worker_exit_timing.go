package devorchestrator

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// The exit-status timing contract (#243), and the at-fork identity that makes
// a Worker that is already gone recordable (#133).
//
// run-worker.sh (renderReaper, worker_guard.go) writes the exit code to disk
// BEFORE it collects the Worker's process group, because a cleanup that fails
// must never be able to cost the record of how the Worker ended. That order is
// deliberate and it is not up for renegotiation, but it has a consequence
// every reader has to respect: from the instant the file appears, its writer
// (the session leader) is still running its cleanup — "kill -TERM", "sleep 1",
// "kill -KILL" over the Worker's group — so *the file is strictly weaker than
// "the run is over"*. A reader that treats the file as completion describes a
// session that is still being torn down; the test that watched `worker stop`
// return into a deleted temp dir caught exactly that (#243).
//
// So the fact this package routes everything through is not the file but the
// writer: a run is over when the session leader that writes the code has
// stopped running. Note *stopped running*, not *gone*: spawn Releases the
// reaper rather than waiting for it (the reaper outlives rddev by design), so
// on a long-lived rddev nobody reaps it and it sits in /proc as a zombie for
// the rest of the session — an existence test on it answers "alive" forever
// (pidRunning, worker_registry.go, tests the /proc state field instead). Every
// path that merges the code — deriveStatus, reconcileWorker, StopWorker —
// reaches that conclusion the same way, so the two halves of #207/243 cannot
// disagree: the acceptance instrument that waits (fg_wait_exit) waits on the
// recorded exit_status, which reconcileWorker only writes after it has seen
// the writer stop.
//
// The sessionResidue exclusion (worker_proc.go) rests on the same reading, and
// its old comment had it backwards: the reaper is not left out because it "may
// still be writing exit.status" — the ordering above is exactly why the file
// says nothing about where the run is. It is left out because it is the run's
// own machinery: the session leader that scopes precisely the processes this
// run started, so it can never be residue the Worker left behind, and naming
// it would refuse a run over a process rddev itself spawned. pid equality with
// the recorded session leader is what carries that, plus the cmdline check so
// a recycled pid that happens to lead the session cannot hide behind it.
//
// The identity half (#133) follows from the same fact. rddev records the
// Worker's pid AND its process start time (as the registry needs both to tell
// a live Worker from a recycled pid), and it used to read both out of
// /proc/<pid> directly after the reaper reported the pid. A Worker can return
// before that read: `review spawn` then failed with "reading
// /proc/<pid>/environ: no such file or directory" while every side effect of
// the run had already landed, and the gate that rules on it only ever said
// "review spawn exited 1". Nothing about a finished process is recoverable
// after it exits, so the reaper — alive for the whole window above, and the
// parent whose environment the Worker inherited — reports what it read at the
// fork. That is the one instant the Worker provably exists.

// spawnHandoff is what the reaper reports about the process it just forked,
// on its stdout line: the Worker's pid, and the process start time (/proc/
// <pid>/stat field 22) the reaper read at that instant. StartTime is 0 when
// the reaper could not read it; the pid is the run's identity and is never 0.
type spawnHandoff struct {
	PID       int
	StartTime uint64
}

// parseSpawnHandoff reads the reaper's identity line, "<pid> <starttime>".
// The start time is an optimization, not a requirement: 0 means the reaper
// could not read /proc/<pid>/stat, and spawn then reads it itself — which is
// what spawn did before the handoff existed. The pid must be there and must be
// positive; a line that does not parse reports ok=false, and the caller treats
// it exactly like a missing report.
func parseSpawnHandoff(line string) (spawnHandoff, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return spawnHandoff{}, false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return spawnHandoff{}, false
	}
	h := spawnHandoff{PID: pid}
	if len(fields) > 1 {
		if ticks, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
			h.StartTime = ticks
		}
	}
	return h, true
}

// spawnStartTime is the start time to record for a just-spawned process: the
// value the reaper read at the fork, or — for a reaper that could not read
// /proc, and for the tests that drive this without one — a direct read here.
// The fallback fails when the process is already gone, which is the case the
// handoff exists to cover; it is kept because the handoff is an optimization
// and a spawn must not depend on the reaper having managed the read.
func spawnStartTime(h spawnHandoff, pid int) (uint64, error) {
	if h.StartTime != 0 {
		return h.StartTime, nil
	}
	return procStartTime(pid)
}

// assertSpawnedEnv is the post-spawn environment assertion of T0011 Defect 1:
// the real environments of the just-started Worker (or Reviewer) and its
// reaper wrapper must carry none of the stripped credential variables.
//
// The Worker is looked at first, and it may already be gone. A process that
// has exited holds nothing, and refusing the spawn for it — which is what the
// inline loop used to do, fail-closed on the read error — refuses a run that
// has already happened and loses its record with it (#133: a Reviewer that
// finished before rddev could read its /proc entry made `review spawn` exit 1
// while every side effect of the run had landed). What still has to be
// witnessed is the environment the Worker was *started with*, and the witness
// that is always there is the reaper: rddev's own child, alive for at least
// its cleanup window after the Worker exits, and the parent whose environment
// the Worker inherited. So a missing Worker is not a failure; a readable
// environment that leaks a credential still is, and a reaper that cannot be
// read (spawn machinery that does not outlive its own child) is still a
// failure.
func assertSpawnedEnv(workerPID, sessionLeaderPID int) error {
	for _, pid := range []int{workerPID, sessionLeaderPID} {
		environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		if err != nil {
			if pid == workerPID && os.IsNotExist(err) {
				continue // the Worker exited before the read; the reaper witnesses it
			}
			return fmt.Errorf("reading /proc/%d/environ: %w", pid, err)
		}
		if leak := assertCleanWorkerEnv(environ); leak != "" {
			return fmt.Errorf("%s is present in the environment of pid %d", leak, pid)
		}
	}
	return nil
}
