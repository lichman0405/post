// Package workerrecovery is the controlled stop/respawn sample for WEB-28
// (T1334): the smallest Go test package that proves a Worker re-dispatched
// from on-disk state can finish the task it was stopped in the middle of. It
// is not product code and makes no claim about platform behaviour.
//
// The drill is modelled with real processes, not by inspecting JSON the test
// wrote itself. Attempt 1 is a child process that records a fresh random
// nonce in a state file, starts a detached grandchild and blocks. The parent
// stops the whole process group plus that grandchild, then polls raw pid
// checks (/proc and syscall.Kill) until both are gone, reaping attempt 1 only
// after that, so a missed kill is observed as a live process. It then starts
// attempt 2, which must read attempt 1's state, echo its nonce and write the
// probe. A stop that leaves a survivor, or a "respawn" that starts from
// scratch instead of reading the disk state, makes the suite fail.
//
// The probe is generated at test time from that nonce, so no fixture
// committed with the patch can satisfy it. The boundary: this proves the
// mechanics a respawned process needs; the real `rddev worker stop` /
// `rddev worker respawn` around this Worker and the final collect are
// Supervisor-side evidence, not something this package asserts.
package workerrecovery

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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

const (
	taskID = "T1334"
	scope  = "tests/worker-recovery"

	// Helper modes for the re-executed test binary.
	helperTest       = "TestAttemptHelperProcess"
	helperEnv        = "WORKER_RECOVERY_HELPER"
	stateEnv         = "WORKER_RECOVERY_STATE"
	modeEnv          = "WORKER_RECOVERY_MODE"
	probeEnv         = "WORKER_RECOVERY_PROBE"
	grandchildPidEnv = "WORKER_RECOVERY_GRANDCHILD_PID"

	modeHold       = "hold"
	modeResume     = "resume"
	modeGrandchild = "grandchild"

	// startTimeout bounds how long the parent waits for an attempt to show up
	// on disk. It fails the stated property (the attempt never recorded
	// itself); it is not a test timeout and does not mask slowness.
	startTimeout = 5 * time.Second

	// stopTimeout bounds how long the parent waits for a stopped pid to go
	// away. It is deliberately shorter than the helpers' sleeps so that a
	// stop which leaves a survivor fails instead of waiting for natural exit.
	stopTimeout = 5 * time.Second

	// Bounded lifetimes: the parent stops these in milliseconds, but if the
	// whole test binary is killed mid-run the helpers still exit instead of
	// lingering for the rest of the drill. holdDuration must stay well above
	// stopTimeout so that a stop which fails to kill attempt 1 is observed as
	// a live process, not masked by the hold expiring on its own.
	holdDuration       = 60 * time.Second
	grandchildDuration = 30 * time.Second
)

// attemptState is the on-disk record one attempt leaves for the next. It is
// intentionally tiny: the drill is about the state being on disk, carrying a
// value the next attempt could not guess, and being readable by a fresh
// process.
type attemptState struct {
	Attempt     int    `json:"attempt"`
	Status      string `json:"status"`
	Scope       string `json:"scope"`
	Nonce       string `json:"nonce"`
	ResumedFrom int    `json:"resumed_from,omitempty"`
}

// TestRespawnedAttemptWritesProbeFromDiskState covers the probe half: attempt
// 1 records a random nonce and is stopped; attempt 2 must read that nonce and
// write the probe from it. The expected probe is built from the nonce the
// parent read off disk after attempt 1, not from anything attempt 2 reports,
// so a from-scratch restart that invents attempt=2 cannot pass.
func TestRespawnedAttemptWritesProbeFromDiskState(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "attempt.json")
	probePath := filepath.Join(dir, "probe.txt")
	exe := testBinary(t)

	hold := startHold(t, exe, statePath, "")
	first := waitForState(t, statePath, 1, "running")
	if first.Nonce == "" {
		t.Fatalf("attempt 1 recorded no nonce on disk; the resume cannot be checked for a disk read")
	}

	stopAttemptGroup(t, hold, 0)
	waitGone(t, hold.cmd.Process.Pid, "attempt 1 after the stop")
	reapAttempt(t, hold)

	runResume(t, exe, statePath, probePath)

	gotProbe, err := os.ReadFile(probePath)
	if err != nil {
		t.Fatalf("read the probe the respawned attempt wrote: %v", err)
	}
	wantProbe := probeContent(first.Attempt+1, first.Attempt, first.Nonce)
	if string(gotProbe) != wantProbe {
		t.Fatalf("probe written after respawn does not echo attempt 1's nonce\n got: %q\nwant: %q", gotProbe, wantProbe)
	}

	second := readStateFile(t, statePath)
	if second.Attempt != 2 || second.ResumedFrom != 1 || second.Nonce != first.Nonce || second.Status != "resumed" {
		t.Fatalf("state after respawn = %+v, want attempt=2 resumed_from=1 status=resumed nonce=%q", second, first.Nonce)
	}
}

// TestStopLeavesNoLiveProcessAndRespawnStartsANewAttempt starts attempt 1
// together with a detached (setsid) grandchild, confirms both are alive with
// raw pid checks, stops the process group and the grandchild, polls raw
// liveness until both are gone, and only then reaps attempt 1 and respawns.
// Removing the group kill, leaving the detached grandchild unstopped, or
// fabricating attempt 2's state all make it fail.
func TestStopLeavesNoLiveProcessAndRespawnStartsANewAttempt(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "attempt.json")
	probePath := filepath.Join(dir, "probe.txt")
	grandchildPidPath := filepath.Join(dir, "grandchild.pid")
	exe := testBinary(t)

	hold := startHold(t, exe, statePath, grandchildPidPath)
	// Register the grandchild cleanup before waiting for its pid: if the pid
	// file never appears (or the test fails first), cleanup still reads what
	// attempt 1 published and kills the survivor. startHold's own cleanup
	// stops the group.
	t.Cleanup(func() {
		if pid, err := readPidFile(grandchildPidPath); err == nil && processAlive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	first := waitForState(t, statePath, 1, "running")
	grandchildPid := waitForPidFile(t, grandchildPidPath)

	// BEFORE: the process and its grandchild must be genuinely alive, or a
	// subsequent "they are gone" check would prove nothing.
	assertAlive(t, hold.cmd.Process.Pid, "attempt 1 before the stop")
	assertAlive(t, grandchildPid, "detached grandchild before the stop")

	stopAttemptGroup(t, hold, grandchildPid)

	// AFTER: raw pid checks, not os.Process state (which Go short-circuits to
	// ErrProcessDone after Wait without asking the OS). The attempt is still
	// unreaped here, so a stop that missed it would be visible as a live
	// process rather than a zombie.
	waitGone(t, hold.cmd.Process.Pid, "attempt 1 after the stop")
	waitGone(t, grandchildPid, "detached grandchild after the stop")
	reapAttempt(t, hold)

	// The respawn must start from the bytes on disk: it has to echo the nonce
	// attempt 1 wrote, and that nonce is not present anywhere else.
	runResume(t, exe, statePath, probePath)

	second := readStateFile(t, statePath)
	if second.Attempt != 2 {
		t.Fatalf("state after respawn: attempt=%d, want 2 (the new attempt must increment the on-disk attempt)", second.Attempt)
	}
	if second.ResumedFrom != 1 {
		t.Fatalf("state after respawn: resumed_from=%d, want 1 (the new attempt must have read attempt 1 from disk)", second.ResumedFrom)
	}
	if second.Nonce != first.Nonce {
		t.Fatalf("state after respawn: nonce=%q, want attempt 1's nonce %q (the new attempt must have read the disk state, not invented one)", second.Nonce, first.Nonce)
	}
	if second.Status != "resumed" {
		t.Fatalf("state after respawn: status=%q, want %q", second.Status, "resumed")
	}

	gotProbe, err := os.ReadFile(probePath)
	if err != nil {
		t.Fatalf("read the probe the respawned attempt wrote: %v", err)
	}
	if want := probeContent(2, 1, first.Nonce); string(gotProbe) != want {
		t.Fatalf("probe written after respawn does not echo attempt 1's nonce\n got: %q\nwant: %q", gotProbe, want)
	}
}

// TestAttemptHelperProcess is the body of an attempt. It runs only when the
// parent test re-executes the binary with helperEnv set; under the normal
// `go test ./tests/worker-recovery/` run it returns without doing anything.
func TestAttemptHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		return
	}

	switch os.Getenv(modeEnv) {
	case modeGrandchild:
		// A detached grandchild that outlives its parent unless the stop
		// reaches it explicitly.
		time.Sleep(grandchildDuration)
		return
	case modeHold:
		statePath := requireEnv(t, stateEnv)

		// Attempt 1 starts from nothing and writes a per-run nonce that only
		// the disk can carry to attempt 2.
		nonce, err := newNonce()
		if err != nil {
			t.Fatalf("attempt 1 cannot generate a nonce: %v", err)
		}
		if err := writeState(statePath, attemptState{Attempt: 1, Status: "running", Scope: scope, Nonce: nonce}); err != nil {
			t.Fatalf("attempt 1 cannot record its state on disk: %v", err)
		}

		if pidPath := os.Getenv(grandchildPidEnv); pidPath != "" {
			exe, err := os.Executable()
			if err != nil {
				t.Fatalf("attempt 1 cannot locate its own binary: %v", err)
			}
			gc := exec.Command(exe, "-test.run=^"+helperTest+"$")
			gc.Env = childEnv(helperEnv, "1", modeEnv, modeGrandchild)
			gc.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := gc.Start(); err != nil {
				t.Fatalf("attempt 1 cannot start its detached grandchild: %v", err)
			}
			// Deliberately not Wait(): this grandchild is meant to survive
			// attempt 1 unless the stop reaches it.
			if err := os.WriteFile(pidPath, []byte(strconv.Itoa(gc.Process.Pid)), 0o600); err != nil {
				t.Fatalf("attempt 1 cannot publish its grandchild pid: %v", err)
			}
		}

		time.Sleep(holdDuration) // the parent stops this in milliseconds
	case modeResume:
		statePath := requireEnv(t, stateEnv)
		probePath := requireEnv(t, probeEnv)

		// Attempt 2 must start from the bytes the stopped attempt left on
		// disk. It cannot know the nonce any other way.
		prev, err := readState(statePath)
		if err != nil {
			t.Fatalf("respawned attempt cannot read the state left on disk: %v", err)
		}
		if prev.Attempt != 1 || prev.Status != "running" {
			t.Fatalf("respawned attempt read unexpected attempt-1 state: %+v", prev)
		}
		if prev.Nonce == "" {
			t.Fatalf("attempt 1 left no nonce on disk; the resume cannot prove it read the state")
		}

		next := attemptState{
			Attempt:     prev.Attempt + 1,
			Status:      "resumed",
			Scope:       prev.Scope,
			Nonce:       prev.Nonce,
			ResumedFrom: prev.Attempt,
		}
		if err := writeState(statePath, next); err != nil {
			t.Fatalf("respawned attempt cannot record attempt 2 on disk: %v", err)
		}
		if err := os.WriteFile(probePath, []byte(probeContent(next.Attempt, next.ResumedFrom, next.Nonce)), 0o600); err != nil {
			t.Fatalf("respawned attempt cannot write the probe: %v", err)
		}
		fmt.Printf("RESUMED attempt=%d from=%d nonce=%s\n", next.Attempt, next.ResumedFrom, next.Nonce)
	default:
		t.Fatalf("unknown helper mode %q", os.Getenv(modeEnv))
	}
}

// heldAttempt carries the attempt process and its captured output so a failed
// stop can report what the attempt printed.
type heldAttempt struct {
	cmd *exec.Cmd
	out *bytes.Buffer
}

func testBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary to re-execute: %v", err)
	}
	return exe
}

// startHold starts attempt 1 in its own process group. An empty
// grandchildPidPath starts no grandchild.
func startHold(t *testing.T, exe, statePath, grandchildPidPath string) *heldAttempt {
	t.Helper()
	cmd := exec.Command(exe, "-test.run=^"+helperTest+"$")
	cmd.Env = childEnv(
		helperEnv, "1",
		stateEnv, statePath,
		modeEnv, modeHold,
		grandchildPidEnv, grandchildPidPath,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	out := &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start attempt 1: %v", err)
	}
	attempt := &heldAttempt{cmd: cmd, out: out}
	t.Cleanup(func() {
		// An attempt that already stopped is already gone; killing the group
		// again only matters when the test failed before the stop.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return attempt
}

// stopAttemptGroup is the stop under test: it sends SIGKILL to attempt 1's
// whole process group and, because the grandchild deliberately left that
// group with setsid, to the grandchild by pid too. It deliberately does NOT
// Wait: the caller must poll the raw pids first. A blocking Wait here would
// reap a process this stop failed to kill and hide the failure, because the
// hold would expire and Wait would then return a dead pid to the AFTER
// checks. Dropping either kill leaves a survivor and the caller's AFTER
// checks fail.
func stopAttemptGroup(t *testing.T, attempt *heldAttempt, grandchildPid int) {
	t.Helper()
	pid := attempt.cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("stop attempt 1's process group %d: %v", pid, err)
	}
	if grandchildPid > 0 {
		if err := syscall.Kill(grandchildPid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("stop detached grandchild %d: %v", grandchildPid, err)
		}
	}
}

// reapAttempt consumes attempt 1's exit status after its raw pid has been
// observed dead. The stop is expected to kill it, so a clean exit is itself a
// failure (it would mean the process ended on its own, e.g. the hold expired).
func reapAttempt(t *testing.T, attempt *heldAttempt) {
	t.Helper()
	if err := attempt.cmd.Wait(); err == nil {
		t.Fatalf("stopped attempt 1 exited with status 0; the stop was expected to kill it")
	}
}

func runResume(t *testing.T, exe, statePath, probePath string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe, "-test.run=^"+helperTest+"$")
	cmd.Env = childEnv(
		helperEnv, "1",
		stateEnv, statePath,
		probeEnv, probePath,
		modeEnv, modeResume,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("respawned attempt failed: %v (timeout: %v)\noutput:\n%s", err, ctx.Err(), out)
	}
}

func waitForState(t *testing.T, path string, wantAttempt int, wantStatus string) attemptState {
	t.Helper()
	deadline := time.Now().Add(startTimeout)
	for time.Now().Before(deadline) {
		if st, err := readState(path); err == nil && st.Attempt == wantAttempt && st.Status == wantStatus {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, err := readState(path)
	t.Fatalf("attempt %d (%s) never appeared on disk within %s (last state: %+v, err: %v)", wantAttempt, wantStatus, startTimeout, st, err)
	return attemptState{}
}

func waitForPidFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(startTimeout)
	for time.Now().Before(deadline) {
		if pid, err := readPidFile(path); err == nil {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("attempt 1 never published its grandchild pid to %s within %s", path, startTimeout)
	return 0
}

// readPidFile reads a pid attempt 1 published. It is used both by the test
// and by cleanup, so it must be safe to call when the file does not exist yet.
func readPidFile(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("parse pid file %s: %w", path, err)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("pid file %s holds non-positive pid %d", path, pid)
	}
	return pid, nil
}

// assertAlive requires the raw pid to exist and not be a zombie.
func assertAlive(t *testing.T, pid int, what string) {
	t.Helper()
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("%s (pid %d) must be alive before the stop, but syscall.Kill(pid, 0) = %v", what, pid, err)
	}
	if !processAlive(pid) {
		t.Fatalf("%s (pid %d) is not a live process before the stop", what, pid)
	}
}

// waitGone polls raw pid liveness and fails if the process survives the stop.
func waitGone(t *testing.T, pid int, what string) {
	t.Helper()
	deadline := time.Now().Add(stopTimeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s (pid %d) is still alive %s after the stop; the stop must leave no live process (state: %s)", what, pid, stopTimeout, procState(pid))
}

// processAlive reports whether pid is a live process: syscall.Kill(pid, 0)
// succeeds and /proc/<pid>/stat does not show a zombie. Zombies are dead even
// though the pid is still reserved, so they must not count as survivors.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		if errors.Is(err, syscall.EPERM) {
			return true // exists, owned by someone else
		}
		return false // ESRCH and friends
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	i := bytes.LastIndexByte(raw, ')')
	if i < 0 || i+2 >= len(raw) {
		return true
	}
	return raw[i+2] != 'Z'
}

// procState is diagnostic only: the single-letter state from /proc/<pid>/stat.
func procState(pid int) string {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "unreadable (" + err.Error() + ")"
	}
	i := bytes.LastIndexByte(raw, ')')
	if i < 0 || i+2 >= len(raw) {
		return "unparseable"
	}
	return string(raw[i+2])
}

// childEnv builds a child environment explicitly: the parent's values for the
// keys this drill sets are dropped, then exactly one value per key is added.
// Appending to os.Environ would leave the parent's value in place too and make
// the effective value depend on lookup order.
func childEnv(pairs ...string) []string {
	replace := make(map[string]bool, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		replace[pairs[i]] = true
	}
	env := make([]string, 0, len(os.Environ())+len(pairs))
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if replace[key] {
			continue
		}
		env = append(env, kv)
	}
	for i := 0; i+1 < len(pairs); i += 2 {
		env = append(env, pairs[i]+"="+pairs[i+1])
	}
	return env
}

func requireEnv(t *testing.T, key string) string {
	t.Helper()
	value := os.Getenv(key)
	if value == "" {
		t.Fatalf("attempt is missing required environment variable %s", key)
	}
	return value
}

func newNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// probeContent is the probe attempt 2 must write. attempt is the new attempt
// number, resumedFrom the attempt it read from disk, and nonce the value it
// read there.
func probeContent(attempt, resumedFrom int, nonce string) string {
	return fmt.Sprintf("WEB-28 worker recovery probe\ntask=%s\nattempt=%d\nresumed_from=%d\nnonce=%s\n",
		taskID, attempt, resumedFrom, nonce)
}

func readStateFile(t *testing.T, path string) attemptState {
	t.Helper()
	st, err := readState(path)
	if err != nil {
		t.Fatalf("read state %s: %v", path, err)
	}
	return st
}

func readState(path string) (attemptState, error) {
	var st attemptState
	raw, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("parse %s: %w", path, err)
	}
	return st, nil
}

func writeState(path string, st attemptState) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}
