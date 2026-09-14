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
// it waits for the reaper to record exit.status afterwards.
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
	// Wait for the reaper to record exit.status; escalate to SIGKILL after
	// the grace period, re-checking liveness before each signal.
	deadline := time.Now().Add(stopGrace)
	for time.Now().Before(deadline) {
		if exitStatusFileExists(rec) {
			break
		}
		if !pidAlive(rec.PID, rec.StartTime) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if exitStatusFileExists(rec) {
		return recordStop(repoRoot, rec)
	}
	if pidAlive(rec.PID, rec.StartTime) {
		if err := signalProcessGroup(rec.PID, syscall.SIGKILL); err != nil {
			return fmt.Errorf("SIGKILL to Worker %s (pid %d): %w", rec.TaskID, rec.PID, err)
		}
		// give the reaper a moment to write exit.status
		time.Sleep(500 * time.Millisecond)
	}
	return recordStop(repoRoot, rec)
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
