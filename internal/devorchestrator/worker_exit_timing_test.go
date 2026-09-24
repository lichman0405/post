package devorchestrator

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The timing contract behind #243 / #207: run-worker.sh writes exit.status
// BEFORE it collects the Worker's process group, so the file is strictly weaker
// than "the run is over" — the writer is still alive while it is on disk. Every
// test below *constructs* that state (a stand-in reaper whose lifetime the test
// picks, never a lucky re-run of a real one) and asserts the reader does not
// read the file as completion.

// fakeRecord is a registry record for a run whose Worker process is already
// gone: the state the reaper is in while it is cleaning up.
func fakeRecord(t *testing.T, repo, taskID string) *WorkerRecord {
	t.Helper()
	dir := WorkerTaskDir(repo, taskID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "worker.log")
	if err := os.WriteFile(logPath, []byte("{\"type\":\"system\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gone, start := exitedProcess(t)
	rec := &WorkerRecord{
		TaskID: taskID, RunID: "run-fake-" + taskID, SessionID: "s-fake", ClaudeVersion: "fake",
		PID: gone, StartTime: start,
		Worktree: filepath.Join(WorktreesDir(repo), taskID), Branch: "task/" + taskID + "-x",
		BaselineSHA: "0123456789abcdef", RefsBefore: []string{},
		LogPath: logPath, ResultDir: dir, StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := SaveRegistry(repo, rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// exitedProcess starts a process, lets it finish and reaps it, and returns its
// pid together with the starttime read while it was alive. Nothing is running
// under that pid any more, so a liveness check on it answers "not running" —
// exactly how the Worker looks to a reader that lands in the reaper's cleanup
// window.
func exitedProcess(t *testing.T) (int, uint64) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	st, err := procStartTime(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid, st
}

// startFakeReaper launches a stand-in for run-worker.sh that reproduces its
// write order and its lifetime: the authoritative copy first, then the task-dir
// copy, and only then the cleanup window (the real wrapper spends it running
// kill -TERM, sleep 1, kill -KILL over the Worker's group). The window is the
// test's to choose — a stand-in that exited at the file would not construct the
// window at all, which is why this is a construction and not a race.
func startFakeReaper(t *testing.T, repo, taskID string, code int, window time.Duration) *exec.Cmd {
	t.Helper()
	auth := authoritativeExitStatusPath(repo, taskID)
	local := exitStatusPath(repo, taskID)
	script := fmt.Sprintf("set -u\nmkdir -p %q %q\necho %d > %q\necho %d > %q\nsleep %s\n",
		filepath.Dir(auth), filepath.Dir(local), code, auth, code, local,
		strconv.FormatFloat(window.Seconds(), 'f', 3, 64))
	cmd := exec.Command("/bin/bash", "-c", script)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

// waitUntil polls fn until it returns true, with a deadline this package's
// other tests use for disk facts written by another process.
func waitUntil(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestExitStatusIsNotCompletion: construct the #243 window — the code is on
// disk and the session leader that wrote it is still running — and show the
// derivation refuses to call the run over. Then let the writer stop and show
// the same record reads exited with the code merged: the fix must not turn a
// finished run into a stuck one.
func TestExitStatusIsNotCompletion(t *testing.T) {
	repo := writeFakeRepo(t)
	rec := fakeRecord(t, repo, "T0001")
	reaper := startFakeReaper(t, repo, "T0001", 7, 1500*time.Millisecond)
	rec.SessionLeaderPID = reaper.Process.Pid
	waitUntil(t, "the reaper to record the code", func() bool { return exitStatusFileExists(rec) })

	// the construction really is the window: the file is there and its writer
	// is alive (both halves asserted, so a broken fixture cannot pass as a fix)
	if !pidRunning(reaper.Process.Pid) {
		t.Fatalf("construction failed: reaper pid %d is not running", reaper.Process.Pid)
	}
	if status, _, _ := deriveStatus(rec); status != WorkerStale {
		t.Errorf("deriveStatus with the code on disk and its writer still running = %q, want %q — the file is not completion (#243)",
			status, WorkerStale)
	}

	// the writer stops: the run is over and the code is merged, once.
	_, _ = reaper.Process.Wait()
	waitUntil(t, "the reaper to stop running", func() bool { return !pidRunning(rec.SessionLeaderPID) })
	if status, _, _ := deriveStatus(rec); status != WorkerExited {
		t.Errorf("deriveStatus after the writer stopped = %q, want %q", status, WorkerExited)
	}
	v, err := reconcileWorker(repo, rec)
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != WorkerExited {
		t.Errorf("reconcile after the writer stopped = %q, want %q", v.Status, WorkerExited)
	}
	if v.Record.ExitStatus == nil || *v.Record.ExitStatus != 7 || v.Record.ExitSource != ExitSourceReconciled {
		t.Errorf("exit code after the writer stopped = %v (source %q), want 7 recorded by reconciliation",
			v.Record.ExitStatus, v.Record.ExitSource)
	}
}

// TestReconcileWaitsOutTheCleanupWindow: the reader that merges the code must
// return only once the writer has stopped. The window is 1.5s and the reader is
// called while it is open, so a reader that trusted the file returns with the
// reaper still running — caught by a liveness assertion, not by a timing
// threshold.
func TestReconcileWaitsOutTheCleanupWindow(t *testing.T) {
	repo := writeFakeRepo(t)
	rec := fakeRecord(t, repo, "T0002")
	reaper := startFakeReaper(t, repo, "T0002", 0, 1500*time.Millisecond)
	rec.SessionLeaderPID = reaper.Process.Pid
	waitUntil(t, "the reaper to record the code", func() bool { return exitStatusFileExists(rec) })

	v, err := reconcileWorker(repo, rec)
	if err != nil {
		t.Fatal(err)
	}
	if pidRunning(reaper.Process.Pid) {
		t.Errorf("reconcile returned while the reaper (pid %d) was still running: it read the file as the end of the run (#243)",
			reaper.Process.Pid)
	}
	if v.Status != WorkerExited {
		t.Errorf("reconcile of a finished run = %q, want %q", v.Status, WorkerExited)
	}
	if v.Record.ExitStatus == nil || *v.Record.ExitStatus != 0 {
		t.Errorf("merged exit code = %v, want the 0 the reaper recorded", v.Record.ExitStatus)
	}
}

// TestReconcileNeverInventsCompletion: a writer that outlasts the grace is not
// waited out forever, and the run is still not declared over while it runs. The
// honest answer is stale — the next discovery retries — and no exit code may
// have been merged.
func TestReconcileNeverInventsCompletion(t *testing.T) {
	repo := writeFakeRepo(t)
	rec := fakeRecord(t, repo, "T0003")
	reaper := startFakeReaper(t, repo, "T0003", 9, 20*time.Second)
	rec.SessionLeaderPID = reaper.Process.Pid
	waitUntil(t, "the reaper to record the code", func() bool { return exitStatusFileExists(rec) })

	v, err := reconcileWorker(repo, rec)
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != WorkerStale {
		t.Errorf("a run whose writer is still running read as %q, want %q", v.Status, WorkerStale)
	}
	if v.Record.ExitStatus != nil {
		t.Errorf("exit code %d merged while its writer was still running", *v.Record.ExitStatus)
	}
	onDisk, err := LoadRegistry(repo, "T0003")
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.ExitStatus != nil {
		t.Errorf("the registry records the run as over (exit %d) while its writer still runs", *onDisk.ExitStatus)
	}
	if !pidRunning(reaper.Process.Pid) {
		t.Fatal("the test's own fixture stopped early: nothing was constructed")
	}
}

// TestSpawnEnvReadToleratesAWorkerThatAlreadyExited is #133: a Worker that
// finished before rddev could read its /proc entry used to refuse the spawn
// ("reading /proc/<pid>/environ: no such file or directory"), losing the record
// of a run whose side effects had already landed — 9 times out of 720 in CI,
// and never once with a word of explanation in the gate log. The environment
// the Worker was started with is witnessed through the reaper, which is alive
// for the whole cleanup window; what was actually asserted (no stripped
// credential variable in either process) has not moved.
func TestSpawnEnvReadToleratesAWorkerThatAlreadyExited(t *testing.T) {
	gone, _ := exitedProcess(t)
	gone2, _ := exitedProcess(t)
	clean := liveEnvironment(t, "clean", []string{"PATH=/usr/bin:/bin"})
	dirty := liveEnvironment(t, "dirty", []string{"PATH=/usr/bin:/bin", "GITEA_TOKEN=leaked"})

	// a Worker that exited before the read is not a refusal
	if err := assertSpawnedEnv(gone, clean.Process.Pid); err != nil {
		t.Errorf("assertSpawnedEnv(exited Worker, live reaper) = %v, want nil", err)
	}
	// a leak in the process that witnesses the run is still one
	err := assertSpawnedEnv(gone, dirty.Process.Pid)
	if err == nil || !strings.Contains(err.Error(), "GITEA_TOKEN") {
		t.Errorf("assertSpawnedEnv(exited Worker, leaking reaper) = %v, want a refusal naming GITEA_TOKEN", err)
	}
	// and a Worker that is still there is still read directly
	err = assertSpawnedEnv(dirty.Process.Pid, clean.Process.Pid)
	if err == nil || !strings.Contains(err.Error(), "GITEA_TOKEN") {
		t.Errorf("assertSpawnedEnv(leaking Worker, live reaper) = %v, want a refusal naming GITEA_TOKEN", err)
	}
	// machinery that cannot be witnessed at all stays fail-closed
	if err := assertSpawnedEnv(gone, gone2); err == nil {
		t.Error("assertSpawnedEnv(exited Worker, exited reaper) = nil, want a refusal — nothing witnessed the run")
	}
}

// liveEnvironment starts a long-lived child whose environment is env plus the
// marker var naming this fixture, so the test knows what the /proc read will
// find there.
//
// The wait is on the MARKER, not on the file merely being readable. The kernel
// only replaces /proc/<pid>/environ at exec, so between fork and exec the entry
// is already readable and still holds the *parent's* environment — a wait that
// stopped at readability returned while the child was still the test binary's
// own image, and the read then found no GITEA_TOKEN and the leak assertion
// passed for the wrong reason. Measured: 3 failures in 40 runs of this file
// before the marker, 0 after. (The same trap is recorded in tasks/decisions.md
// L1-92's neighbour: the first read of /proc/<pid>/environ comes back empty.)
func liveEnvironment(t *testing.T, marker string, env []string) *exec.Cmd {
	t.Helper()
	const key = "POST_T1215_FIXTURE"
	cmd := exec.Command("sleep", "30")
	cmd.Env = append(append([]string{}, env...), key+"="+marker)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	waitUntil(t, "the fixture process to have exec'd into its own environment", func() bool {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", cmd.Process.Pid))
		if err != nil {
			return false
		}
		return bytes.Contains(b, []byte(key+"="+marker))
	})
	return cmd
}

// TestReaperReportsTheWorkersStartTimeAtTheFork pins the other half of the #133
// handoff. rddev used to read the Worker's start time out of /proc itself,
// right after the reaper reported the pid — a read a Worker that returned first
// makes impossible, and the read that failed took the whole run's record with
// it. So the reaper reports it instead, and the number it reports has to BE
// /proc's field 22: pidAlive compares the recorded value against that field, so
// a line carrying the wrong field (or a mis-counted offset after the comm's
// closing paren) would make every freshly spawned Worker read as a recycled pid
// and every run look stale. The real generated script is run here, so the awk
// is under test and not just the parser.
func TestReaperReportsTheWorkersStartTimeAtTheFork(t *testing.T) {
	dir := t.TempDir()
	reaper := filepath.Join(dir, "run-worker.sh")
	if err := os.WriteFile(reaper, []byte(reaperScript), 0o755); err != nil {
		t.Fatal(err)
	}
	// A Worker that outlives the read, so the value the reaper reported can be
	// compared against the one /proc still serves.
	cmd := exec.Command("bash", reaper, dir, filepath.Join(dir, "worker.log"),
		filepath.Join(dir, "claude.pid"),
		filepath.Join(dir, "exit.status"),
		filepath.Join(dir, "authoritative", "exit.status"), "bash", "-c", "sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if raw, err := os.ReadFile(filepath.Join(dir, "claude.pid")); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				_ = signalProcessGroup(pid, syscall.SIGKILL)
			}
		}
		_ = signalProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})

	sc := bufio.NewScanner(stdout)
	if !sc.Scan() {
		t.Fatal("the reaper printed no line on stdout — spawn reads the Worker identity from there and would abort after 10s")
	}
	h, ok := parseSpawnHandoff(sc.Text())
	if !ok {
		t.Fatalf("the reaper's stdout line %q does not parse as a spawn handoff", sc.Text())
	}
	if h.StartTime == 0 {
		t.Fatalf("the reaper reported no start time (%q): rddev would fall back to a /proc read it exists to avoid (#133)", sc.Text())
	}
	want, err := procStartTime(h.PID)
	if err != nil {
		t.Fatalf("reading /proc starttime for the Worker pid %d: %v", h.PID, err)
	}
	if h.StartTime != want {
		t.Errorf("the reaper reported start time %d, but /proc/%d/stat field 22 is %d — pidAlive compares the recorded value against that field, so every Worker would read as a recycled pid",
			h.StartTime, h.PID, want)
	}
}

// TestParseSpawnHandoffAcceptsTheOldPidOnlyLine: the handoff is an addition to
// the reaper's stdout contract, and a reaper generated BEFORE it (one already
// running, or a run started by an older rddev) prints the bare pid. That line
// must still spawn — the start time falls back to a direct /proc read, which is
// what every spawn did before the handoff existed.
func TestParseSpawnHandoffAcceptsTheOldPidOnlyLine(t *testing.T) {
	h, ok := parseSpawnHandoff("4242\n")
	if !ok || h.PID != 4242 {
		t.Fatalf("parseSpawnHandoff(\"4242\") = %+v ok=%v, want pid 4242", h, ok)
	}
	if h.StartTime != 0 {
		t.Errorf("a pid-only line reported start time %d, want 0 (the caller then reads /proc itself)", h.StartTime)
	}
	// and a line that is not a pid at all is refused rather than guessed at
	for _, bad := range []string{"", "  ", "not-a-pid", "0", "-3", "run-worker: starting"} {
		if _, ok := parseSpawnHandoff(bad); ok {
			t.Errorf("parseSpawnHandoff(%q) accepted a line that names no Worker pid", bad)
		}
	}
}
