package devorchestrator

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeFakeRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".rddev", "workers"), 0o755); err != nil {
		t.Fatal(err)
	}
	return repo
}

// TestRegistryRoundTrip: save, reload, and the fields survive verbatim.
func TestRegistryRoundTrip(t *testing.T) {
	repo := writeFakeRepo(t)
	budget := 2.5
	rec := &WorkerRecord{
		TaskID: "T0001", RunID: "run-abc", SessionID: "s-1",
		ClaudeVersion: "2.1.269 (Claude Code)", Model: "deepseek-v4-pro",
		MaxBudgetUSD: &budget, PID: 4242, StartTime: 7,
		Worktree: "/x", Branch: "task/T0001-x", BaselineSHA: "0123456789abcdef",
		RefsBefore: []string{"refs/heads/main 0123456789abcdef"},
		LogPath:    filepath.Join(WorkersDir(repo), "T0001", "worker.log"),
		ResultDir:  WorkerTaskDir(repo, "T0001"),
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if err := SaveRegistry(repo, rec); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRegistry(repo, "T0001")
	if err != nil {
		t.Fatal(err)
	}
	if got.TaskID != "T0001" || got.RunID != "run-abc" || got.PID != 4242 || got.Model != "deepseek-v4-pro" {
		t.Errorf("round trip lost fields: %+v", got)
	}
	if got.MaxBudgetUSD == nil || *got.MaxBudgetUSD != 2.5 {
		t.Errorf("budget not preserved: %v", got.MaxBudgetUSD)
	}
	if len(got.RefsBefore) != 1 || got.RefsBefore[0] != "refs/heads/main 0123456789abcdef" {
		t.Errorf("refs snapshot not preserved: %v", got.RefsBefore)
	}
	// the file is the schema-valid shape: required fields present
	var raw map[string]any
	data, err := os.ReadFile(registryPath(repo, "T0001"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("registry is not valid JSON: %v", err)
	}
	for _, key := range []string{"task_id", "run_id", "session_id", "claude_version", "pid", "start_time", "worktree", "branch", "baseline_sha", "refs_before", "log_path", "result_dir", "started_at"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("registry missing required field %q", key)
		}
	}
}

// TestRegistryConcurrentSaves: concurrent writers never leave a torn file.
func TestRegistryConcurrentSaves(t *testing.T) {
	repo := writeFakeRepo(t)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := &WorkerRecord{TaskID: "T0001", RunID: "run-" + strconv.Itoa(i), SessionID: "s", ClaudeVersion: "v", PID: i + 1, StartTime: uint64(i + 1), Worktree: "/x", Branch: "b", BaselineSHA: "0123456789abcdef", RefsBefore: []string{}, LogPath: "/l", ResultDir: "/r", StartedAt: "t"}
			if err := SaveRegistry(repo, rec); err != nil {
				t.Errorf("concurrent save: %v", err)
			}
		}(i)
	}
	wg.Wait()
	got, err := LoadRegistry(repo, "T0001")
	if err != nil {
		t.Fatalf("registry torn by concurrent writes: %v", err)
	}
	if got == nil || got.RunID == "" {
		t.Fatal("registry empty after concurrent writes")
	}
}

// TestPidAlive: liveness pairs pid with /proc starttime, so pid reuse reads
// as dead.
func TestPidAlive(t *testing.T) {
	// the test process itself is alive with its real starttime
	st, err := procStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if !pidAlive(os.Getpid(), st) {
		t.Error("pidAlive(self, real starttime) = false, want true")
	}
	// wrong starttime => treated as dead even though the pid exists
	if pidAlive(os.Getpid(), st+1) {
		t.Error("pidAlive(self, wrong starttime) = true, want false (pid reuse must read dead)")
	}
	// a process that exited => dead
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadSt, err := procStartTime(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if !pidAlive(cmd.Process.Pid, deadSt) {
		t.Error("pidAlive(live child, real starttime) = false, want true")
	}
	cmd.Wait()
	if pidAlive(cmd.Process.Pid, deadSt) {
		t.Error("pidAlive(exited child) = true, want false")
	}
}

// TestDiscoverWorkersRestartDiscovery: a fresh process derives running /
// exited / stale purely from disk — the Supervisor-restart scenario.
func TestDiscoverWorkersRestartDiscovery(t *testing.T) {
	repo := writeFakeRepo(t)

	// running: our own pid with its real starttime
	selfSt, err := procStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	// exited: dead pid + exit.status recorded by the reaper
	dead := exec.Command("sh", "-c", "exit 0")
	if err := dead.Start(); err != nil {
		t.Fatal(err)
	}
	deadSt, _ := procStartTime(dead.Process.Pid)
	dead.Wait()
	// stale: dead pid, no exit.status
	gone := exec.Command("sh", "-c", "exit 0")
	if err := gone.Start(); err != nil {
		t.Fatal(err)
	}
	goneSt, _ := procStartTime(gone.Process.Pid)
	gone.Wait()

	save := func(taskID string, pid int, st uint64) {
		t.Helper()
		rec := &WorkerRecord{TaskID: taskID, RunID: "run-" + taskID, SessionID: "s", ClaudeVersion: "v", PID: pid, StartTime: st, Worktree: "/w", Branch: "b", BaselineSHA: "0123456789abcdef", RefsBefore: []string{}, LogPath: filepath.Join(WorkerTaskDir(repo, taskID), "worker.log"), ResultDir: WorkerTaskDir(repo, taskID), StartedAt: "t"}
		if err := SaveRegistry(repo, rec); err != nil {
			t.Fatal(err)
		}
	}
	save("T0001", os.Getpid(), selfSt)
	save("T0002", dead.Process.Pid, deadSt)
	save("T0003", gone.Process.Pid, goneSt)
	if err := os.WriteFile(exitStatusPath(repo, "T0002"), []byte("3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	views, err := DiscoverWorkers(repo)
	if err != nil {
		t.Fatal(err)
	}
	byTask := map[string]WorkerView{}
	for _, v := range views {
		byTask[v.Record.TaskID] = v
	}
	if byTask["T0001"].Status != WorkerRunning {
		t.Errorf("T0001 status = %s, want running (live pid + matching starttime)", byTask["T0001"].Status)
	}
	if byTask["T0002"].Status != WorkerExited || byTask["T0002"].Record.ExitStatus == nil || *byTask["T0002"].Record.ExitStatus != 3 {
		t.Errorf("T0002 status = %s exit=%v, want exited with exit 3 merged from exit.status", byTask["T0002"].Status, byTask["T0002"].Record.ExitStatus)
	}
	if byTask["T0003"].Status != WorkerStale {
		t.Errorf("T0003 status = %s, want stale (dead pid, no exit recorded) — a stale Worker must never read as completed", byTask["T0003"].Status)
	}
	// the exit merge was persisted: a second fresh discovery agrees
	rec, err := LoadRegistry(repo, "T0002")
	if err != nil {
		t.Fatal(err)
	}
	if rec.ExitStatus == nil || *rec.ExitStatus != 3 || rec.ExitSource != ExitSourceReconciled {
		t.Errorf("exit.status not persisted into the registry: exit=%v source=%s", rec.ExitStatus, rec.ExitSource)
	}
}

// TestDiscoverWorkersReconcilesALostRun: a Worker whose process *and* whose
// reaper are both gone, with no exit.status on either copy, is over — only
// the number is missing, and the unrecorded sentinel records that. Without
// this the record stayed stale forever and the task had no legal move: collect
// refused for a missing exit status, rework/respawn for a Worker that "has not
// exited", and the driver reported it as still working indefinitely
// (2026-09-21: T0612 sat that way for 7.5 hours).
//
// The guard is the reaper: a record whose session leader was never recorded
// (or is still alive) stays stale, because nothing can show the writer is
// done.
func TestDiscoverWorkersReconcilesALostRun(t *testing.T) {
	repo := writeFakeRepo(t)

	// reaped returns a pid that has already exited, with its real starttime.
	reaped := func() (int, uint64) {
		t.Helper()
		cmd := exec.Command("sh", "-c", "exit 0")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		st, _ := procStartTime(cmd.Process.Pid)
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
		return cmd.Process.Pid, st
	}
	workerPid, workerSt := reaped()
	reaperPid, _ := reaped()

	// a live reaper: the run is not over, whatever the Worker's pid says
	live := exec.Command("sh", "-c", "sleep 30")
	if err := live.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = live.Process.Kill(); _, _ = live.Process.Wait() }()

	save := func(taskID string, leader int) {
		t.Helper()
		rec := &WorkerRecord{TaskID: taskID, RunID: "run-" + taskID, SessionID: "s",
			ClaudeVersion: "v", PID: workerPid, StartTime: workerSt, SessionLeaderPID: leader,
			Worktree: "/w", Branch: "b", BaselineSHA: "0123456789abcdef", RefsBefore: []string{},
			LogPath:   filepath.Join(WorkerTaskDir(repo, taskID), "worker.log"),
			ResultDir: WorkerTaskDir(repo, taskID), StartedAt: "t"}
		if err := SaveRegistry(repo, rec); err != nil {
			t.Fatal(err)
		}
	}
	save("T0101", reaperPid)        // Worker and reaper both gone
	save("T0102", live.Process.Pid) // Worker gone, reaper alive
	save("T0103", 0)                // no reaper recorded: nothing to check

	views, err := DiscoverWorkers(repo)
	if err != nil {
		t.Fatal(err)
	}
	byTask := map[string]WorkerView{}
	for _, v := range views {
		byTask[v.Record.TaskID] = v
	}

	lost := byTask["T0101"]
	if lost.Status != WorkerExited || lost.Record.ExitStatus == nil || *lost.Record.ExitStatus != ExitStatusUnrecorded {
		t.Fatalf("T0101 status=%s exit=%v, want exited with the unrecorded sentinel %d (collect reads %d as the run did not complete)",
			lost.Status, lost.Record.ExitStatus, ExitStatusUnrecorded, ExitStatusUnrecorded)
	}
	if *lost.Record.ExitStatus == 0 {
		t.Fatal("the sentinel must never be 0 — a lost run must not read as a completed one")
	}
	if lost.Record.ExitSource != ExitSourceReconciled {
		t.Errorf("T0101 exit source = %q, want %q", lost.Record.ExitSource, ExitSourceReconciled)
	}
	// the reconstruction is durable and lands in both copies: a later
	// verifyGateInputs compares them, and one present copy against one absent
	// one would read as tampering
	for _, path := range []string{exitStatusPath(repo, "T0101"), authoritativeExitStatusPath(repo, "T0101")} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("exit.status not written to %s: %v", path, err)
		}
		if strings.TrimSpace(string(data)) != strconv.Itoa(ExitStatusUnrecorded) {
			t.Errorf("%s = %q, want %d", path, strings.TrimSpace(string(data)), ExitStatusUnrecorded)
		}
	}
	rec, err := LoadRegistry(repo, "T0101")
	if err != nil {
		t.Fatal(err)
	}
	if rec.ExitStatus == nil || *rec.ExitStatus != ExitStatusUnrecorded || rec.EndedAt == "" {
		t.Errorf("reconcile not persisted: exit=%v ended_at=%q", rec.ExitStatus, rec.EndedAt)
	}

	for _, id := range []string{"T0102", "T0103"} {
		if byTask[id].Status != WorkerStale {
			t.Errorf("%s status = %s, want stale — the run is only over once the reaper that writes the code is shown to be gone", id, byTask[id].Status)
		}
		if got, err := LoadRegistry(repo, id); err != nil || got.ExitStatus != nil {
			t.Errorf("%s: exit status recorded as %v, want none", id, got.ExitStatus)
		}
	}
}

// TestDiscoverWorkersToleratesGarbage: task dirs without a registry or with a
// truncated registry fail loudly (drift), never silently.
func TestDiscoverWorkersTruncatedRegistryFails(t *testing.T) {
	repo := writeFakeRepo(t)
	dir := WorkerTaskDir(repo, "T0001")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// truncated JSON (the L1-20260912-9 spawn.sh failure mode: an empty file)
	if err := os.WriteFile(filepath.Join(dir, "registry.json"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := DiscoverWorkers(repo); err == nil {
		t.Fatal("discovery accepted a truncated registry")
	}
	// a dir without any registry is simply skipped (spawn in flight)
	repo2 := writeFakeRepo(t)
	if err := os.MkdirAll(WorkerTaskDir(repo2, "T0009"), 0o755); err != nil {
		t.Fatal(err)
	}
	views, err := DiscoverWorkers(repo2)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 0 {
		t.Errorf("empty task dir produced %d views, want 0", len(views))
	}
}

// TestRunningWorkers: the parallelism gate counts only live Workers.
func TestRunningWorkers(t *testing.T) {
	repo := writeFakeRepo(t)
	selfSt, err := procStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	rec := &WorkerRecord{TaskID: "T0001", RunID: "r", SessionID: "s", ClaudeVersion: "v", PID: os.Getpid(), StartTime: selfSt, Worktree: "/w", Branch: "b", BaselineSHA: "0123456789abcdef", RefsBefore: []string{}, LogPath: filepath.Join(WorkerTaskDir(repo, "T0001"), "worker.log"), ResultDir: WorkerTaskDir(repo, "T0001"), StartedAt: "t"}
	if err := SaveRegistry(repo, rec); err != nil {
		t.Fatal(err)
	}
	live, err := RunningWorkers(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].Status != WorkerRunning {
		t.Errorf("RunningWorkers = %+v, want exactly the live worker", live)
	}
}

// TestTaskBranch: the task branch name is stable and refs/heads/task/**-shaped.
func TestTaskBranch(t *testing.T) {
	if got := TaskBranch("T0010", "Worktree & Worker scheduling!"); got != "task/T0010-worktree-worker-scheduling" {
		t.Errorf("TaskBranch = %q", got)
	}
	if got := TaskBranch("T0001", "中文标题"); got != "task/T0001-work" {
		t.Errorf("TaskBranch (non-ascii) = %q, want the fallback slug", got)
	}
	if !strings.HasPrefix(TaskBranch("T0010", "x"), "task/T0010-") {
		t.Error("TaskBranch must live in the Supervisor namespace refs/heads/task/**")
	}
}
