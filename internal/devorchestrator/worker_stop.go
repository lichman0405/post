package devorchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// `worker stop` terminates a live Worker and records the exit. Stopping is
// deliberately NOT a state transition: a stopped Worker is exited-with-signal
// and the task stays `running` until collect (T0011) derives
// worker_failed/verification — a crash or a stop can never look like a
// completed task.

// stopGrace is how long stop waits between SIGTERM and SIGKILL, and how long
// it waits (after each signal) for the reaper to finish: writing the code,
// then collecting the Worker's process group.
const stopGrace = 10 * time.Second

// StopWorker signals the recorded Worker (SIGTERM, then SIGKILL after a
// grace) and records the exit status in the registry. It re-verifies pid
// liveness immediately before every signal, so a recycled pid can never be
// signalled by accident; if the recorded pid is already gone, nothing is
// signalled and the stale fact is reconciled instead.
func StopWorker(repoRoot string, rec *WorkerRecord) error {
	if !pidAlive(rec.PID, rec.StartTime) {
		// The Worker died without a recorded exit — reconcile the stale fact
		// rather than signalling a possibly-recycled pid.
		_, err := DiscoverWorkers(repoRoot)
		if err != nil {
			return err
		}
		return fmt.Errorf("worker %s (pid %d) is not running (stale) — nothing was signalled; see `rddev worker list`", rec.TaskID, rec.PID)
	}

	// The group, not the process: the Worker leads its own group (the reaper
	// runs it under job control), and signalling only the leader would leave
	// everything it started running (#166).
	if err := signalProcessGroup(rec.PID, syscall.SIGTERM); err != nil {
		return fmt.Errorf("signalling Worker %s (pid %d): %w", rec.TaskID, rec.PID, err)
	}
	// Wait for the run to be over, then escalate to SIGKILL after the grace,
	// re-checking liveness before the signal.
	//
	// "Over" is the reaper being gone, NOT exit.status existing (#243). The
	// reaper writes the code first and collects the Worker's process group
	// afterwards — deliberately, so a cleanup that fails cannot cost the
	// record — so stopping at the file would hand back a machine that is still
	// tearing the session down: the reaper's cwd is the Worker's worktree, and
	// a caller that deletes that worktree the moment stop returns (a test
	// removing its temp repo) leaves a live process sitting in a deleted
	// directory. The reaper needs a moment after the file appears; that moment
	// is exactly what this wait absorbs.
	if !waitStopOver(rec) && pidAlive(rec.PID, rec.StartTime) {
		if err := signalProcessGroup(rec.PID, syscall.SIGKILL); err != nil {
			return fmt.Errorf("SIGKILL to Worker %s (pid %d): %w", rec.TaskID, rec.PID, err)
		}
		// the reaper still records the code and finishes its own cleanup
		waitStopOver(rec)
	}
	return recordStop(repoRoot, rec)
}

// waitStopOver waits, bounded by stopGrace, for the run to be over: the
// session leader that writes the exit code is gone. For a record that never
// named a reaper there is no writer to wait for, so the code being on disk is
// the whole of the evidence (the same standard reconcileWorker applies).
// Returns whether that state was reached; when it was not, StopWorker
// escalates — and recordStop records what the file says either way, because
// the record of how the Worker ended must not be hostage to a reaper that
// never finishes.
func waitStopOver(rec *WorkerRecord) bool {
	deadline := time.Now().Add(stopGrace)
	for {
		if !reaperLive(rec) && (rec.SessionLeaderPID > 0 || exitStatusFileExists(rec)) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// recordStop merges the reaper's exit.status (or a synthetic -1 when none was
// recorded) into the registry under the registry lock, and writes the same
// code into the Supervisor-owned authoritative dir (T0012) so collect's
// authoritative copy agrees for stopped Workers too.
func recordStop(repoRoot string, rec *WorkerRecord) error {
	code := -1
	data, err := os.ReadFile(filepath.Join(rec.ResultDir, "exit.status"))
	if err == nil {
		var c int
		if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &c); err == nil {
			code = c
		}
	}
	if err := writeAuthoritativeExitStatus(repoRoot, rec.TaskID, code); err != nil {
		return err
	}
	lock, err := lockPathFor(registryPath(repoRoot, rec.TaskID))
	if err != nil {
		return err
	}
	unlock, err := lockFile(lock)
	if err != nil {
		return err
	}
	defer unlock()
	// re-read under the lock so a concurrent reconcile is not clobbered
	cur, err := LoadRegistry(repoRoot, rec.TaskID)
	if err != nil || cur == nil {
		if err != nil {
			return err
		}
		cur = rec
	}
	now := time.Now().UTC().Format(time.RFC3339)
	cur.ExitStatus = &code
	cur.EndedAt = now
	// stop terminated it; the reaper (if it survived) recorded the code
	cur.ExitSource = ExitSourceStop
	cur.ReconciledAt = now
	return writeFileAtomic(registryPath(repoRoot, rec.TaskID), marshalIndentBytes(cur))
}
