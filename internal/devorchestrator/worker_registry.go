package devorchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The worker registry is the disk record of every Worker attempt, one file per
// task at .rddev/workers/<TASK_ID>/registry.json (L1-20260912-5). It is the
// only thing the Supervisor needs after a restart: everything — running, stale
// and exited workers — is re-discovered from disk, never from in-memory state.
//
// Layout (all under .rddev/workers/<TASK_ID>/):
//
//	registry.json      run facts, written by spawn, reconciled on discovery
//	exit.status        claude's exit code, written by the reaper wrapper
//	claude.pid          claude's pid, written by the reaper wrapper
//	worker.log          the claude stream-json log (hang detection = log growth)
//	task-package.json   the validated task package the Worker received
//	prompt.md           rendered worker prompt
//	system.md           rendered worker contract (appended to system prompt)
//	worker-settings.json the generated Claude Code settings (permissions + hook)
//	guard/worker-guard.sh the PreToolUse isolation guard
//	run-worker.sh       the reaper wrapper that outlives rddev
//
// A record never moves between dirs and is never deleted by rddev (gc belongs
// to T0011); `status` is a *derived* view computed at discovery time from the
// recorded facts, so a stale process can never be mistaken for a running one.

// WorkerStatus is the derived liveness view of one recorded attempt.
type WorkerStatus string

const (
	// WorkerRunning: the recorded pid exists and its /proc/<pid>/stat field 22
	// (starttime) matches the recorded start_time — the pid was not reused.
	WorkerRunning WorkerStatus = "running"
	// WorkerExited: the reaper recorded an exit status; the run is over.
	WorkerExited WorkerStatus = "exited"
	// WorkerStale: the pid is gone or reused but no exit status was recorded
	// (rddev or the host died, or the reaper was killed). A stale worker is
	// never treated as completed — collect (T0011) owns that judgement.
	WorkerStale WorkerStatus = "stale"
)

// ExitSource marks who recorded exit_status.
const (
	ExitSourceReaper     = "reaper"     // run-worker.sh wrote exit.status
	ExitSourceStop       = "stop"       // rddev worker stop terminated the run
	ExitSourceReconciled = "reconciled" // discovery merged exit.status into the registry
)

// WorkerRecord is the on-disk registry entry (specs/orchestrator/
// worker-registry.schema.json). start_time is the /proc/<pid>/stat field-22
// process start time in clock ticks — pairing it with the pid defeats pid
// reuse: a recycled pid with a different starttime reads as stale, not running.
type WorkerRecord struct {
	TaskID        string   `json:"task_id"`
	RunID         string   `json:"run_id"`
	SessionID     string   `json:"session_id"`
	ClaudeVersion string   `json:"claude_version"`
	Model         string   `json:"model"`
	Effort        string   `json:"effort"`
	MaxBudgetUSD  *float64 `json:"max_budget_usd,omitempty"`
	MaxTurns      *int     `json:"max_turns,omitempty"`
	Timeout       string   `json:"timeout,omitempty"`
	DockerGrant   bool     `json:"docker_grant,omitempty"`
	PID           int      `json:"pid"`
	StartTime     uint64   `json:"start_time"`
	Worktree      string   `json:"worktree"`
	Branch        string   `json:"branch"`
	BaselineSHA   string   `json:"baseline_sha"`
	RefsBefore    []string `json:"refs_before"`
	LogPath       string   `json:"log_path"`
	ResultDir     string   `json:"result_dir"`
	StartedAt     string   `json:"started_at"`
	EndedAt       string   `json:"ended_at,omitempty"`
	ExitStatus    *int     `json:"exit_status,omitempty"`
	ExitSource    string   `json:"exit_source,omitempty"`
	ReconciledAt  string   `json:"reconciled_at,omitempty"`
}

// WorkerView is a registry entry plus its derived status, as printed by
// `rddev worker list`.
type WorkerView struct {
	Record  WorkerRecord
	Status  WorkerStatus
	LogPath string
	// LogBytes and LogAge are reported for hang detection (L1-20260912-7: a
	// Worker is hung only when its stream log stops growing, never because it
	// spent long without a tool call).
	LogBytes int64
	LogAgeS  int64 // seconds since the log last grew; 0 if not running
}

// WorkersDir returns the per-repo workers directory for repoRoot.
func WorkersDir(repoRoot string) string { return filepath.Join(repoRoot, ".rddev", "workers") }

// WorktreesDir returns the per-repo worktrees directory for repoRoot.
func WorktreesDir(repoRoot string) string { return filepath.Join(repoRoot, ".rddev", "worktrees") }

// WorkerTaskDir returns the per-task runtime directory.
func WorkerTaskDir(repoRoot, taskID string) string {
	return filepath.Join(WorkersDir(repoRoot), taskID)
}

// registryPath returns the registry file for one task.
func registryPath(repoRoot, taskID string) string {
	return filepath.Join(WorkerTaskDir(repoRoot, taskID), "registry.json")
}

// exitStatusPath returns the reaper-written exit.status file for one task.
func exitStatusPath(repoRoot, taskID string) string {
	return filepath.Join(WorkerTaskDir(repoRoot, taskID), "exit.status")
}

// SaveRegistry atomically writes rec (callers hold no lock; the write itself
// is atomic and discovery tolerates a concurrent reconcile, but spawn/stop
// serialize their read-modify-write cycles on the registry lock).
func SaveRegistry(repoRoot string, rec *WorkerRecord) error {
	path := registryPath(repoRoot, rec.TaskID)
	lock, err := lockPathFor(path)
	if err != nil {
		return err
	}
	unlock, err := lockFile(lock)
	if err != nil {
		return err
	}
	defer unlock()
	return writeFileAtomic(path, marshalIndentBytes(rec))
}

func marshalIndentBytes(v any) []byte {
	b, err := marshalIndent(v)
	if err != nil {
		// marshalIndent cannot fail for these structs; keep the signature simple.
		panic(fmt.Sprintf("encoding worker registry: %v", err))
	}
	return b
}

// lockFile takes an exclusive flock on path, creating it if needed. It is the
// same scheme store.go uses (via lockPathFor), so registry updates and state
// updates never invent a second locking discipline.
func lockFile(path string) (func(), error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating lock directory %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) // best effort
		f.Close()
	}, nil
}

// LoadRegistry reads one task's registry file; a missing file is a nil
// record with no error (the task simply has no recorded attempt).
func LoadRegistry(repoRoot, taskID string) (*WorkerRecord, error) {
	data, err := os.ReadFile(registryPath(repoRoot, taskID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading registry for %s: %w", taskID, err)
	}
	var rec WorkerRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("parsing registry for %s: %w", taskID, err)
	}
	if rec.TaskID == "" {
		return nil, fmt.Errorf("registry for %s is missing task_id (truncated or hand-edited?)", taskID)
	}
	return &rec, nil
}

// DiscoverWorkers re-derives every Worker's status from disk: registry files
// are scanned, exit.status files merged in, and pid liveness checked against
// the recorded /proc starttime. This is the only discovery path — it works
// identically for a fresh rddev process (Supervisor restart) and a long-lived
// one, because it never trusts memory.
func DiscoverWorkers(repoRoot string) ([]WorkerView, error) {
	entries, err := os.ReadDir(WorkersDir(repoRoot))
	if os.IsNotExist(err) {
		return []WorkerView{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scanning workers dir: %w", err)
	}
	views := make([]WorkerView, 0)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		rec, err := LoadRegistry(repoRoot, e.Name())
		if err != nil {
			return nil, err
		}
		if rec == nil {
			continue // task dir without a registry yet (spawn in flight)
		}
		v, err := reconcileWorker(repoRoot, rec)
		if err != nil {
			return nil, err
		}
		views = append(views, v)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Record.TaskID < views[j].Record.TaskID })
	return views, nil
}

// reconcileWorker derives one record's status, merging the reaper's
// exit.status into the registry file when the process is gone (so the fact is
// persisted for later reads and for collect/T0011).
func reconcileWorker(repoRoot string, rec *WorkerRecord) (WorkerView, error) {
	v := WorkerView{Record: *rec, LogPath: rec.LogPath}
	v.Status, v.LogBytes, v.LogAgeS = deriveStatus(rec)

	if rec.ExitStatus == nil && v.Status == WorkerExited {
		// The reaper recorded an exit after this registry was written; merge
		// it in so the exit code survives process restarts of the reader.
		code, err := readExitStatus(repoRoot, rec.TaskID)
		if err != nil {
			return v, err
		}
		now := time.Now().UTC().Format(time.RFC3339)
		rec.ExitStatus = &code
		rec.EndedAt = now
		rec.ExitSource = ExitSourceReconciled
		rec.ReconciledAt = now
		if err := SaveRegistry(repoRoot, rec); err != nil {
			return v, fmt.Errorf("reconciling registry for %s: %w", rec.TaskID, err)
		}
		v.Record = *rec
		v.Status = WorkerExited
	}
	return v, nil
}

// deriveStatus decides running/exited/stale from the recorded facts alone.
func deriveStatus(rec *WorkerRecord) (WorkerStatus, int64, int64) {
	if rec.ExitStatus != nil {
		bytes, _ := logStats(rec.LogPath)
		return WorkerExited, bytes, 0
	}
	alive := pidAlive(rec.PID, rec.StartTime)
	if alive {
		bytes, age := logStats(rec.LogPath)
		return WorkerRunning, bytes, age
	}
	bytes, _ := logStats(rec.LogPath)
	if exitStatusFileExists(rec) {
		// the reaper wrote exit.status after the registry snapshot was read;
		// treat as exited so reconcile can merge it
		return WorkerExited, bytes, 0
	}
	return WorkerStale, bytes, 0
}

// pidAlive reports whether pid exists with the recorded process start time.
func pidAlive(pid int, startTime uint64) bool {
	if pid <= 0 {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// field 22 is starttime; fields before it may contain spaces (comm in
	// parens), so parse from the last ')' instead of splitting naively.
	rest := data
	if i := strings.LastIndexByte(string(rest), ')'); i >= 0 {
		rest = rest[i+1:]
	}
	fields := strings.Fields(string(rest))
	// after ")": state(3) r ppid(4) pgrp(5) session(6) tty(7) tpgid(8) flags(9)
	// minflt(10) cminflt(11) majflt(12) cmajflt(13) utime(14) stime(15)
	// cutime(16) cstime(17) priority(18) nice(19) num_threads(20)
	// itrealvalue(21) starttime(22) — field 22 is the 20th field after ')',
	// i.e. index 19.
	if len(fields) < 20 {
		return false
	}
	st, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return false
	}
	return st == startTime
}

// readExitStatus reads the reaper-written exit.status file.
func readExitStatus(repoRoot, taskID string) (int, error) {
	data, err := os.ReadFile(exitStatusPath(repoRoot, taskID))
	if err != nil {
		return 0, fmt.Errorf("reading exit status for %s: %w", taskID, err)
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("exit status for %s is not an integer: %q", taskID, strings.TrimSpace(string(data)))
	}
	return code, nil
}

// exitStatusFileExists reports whether the reaper has written exit.status.
func exitStatusFileExists(rec *WorkerRecord) bool {
	// the file lives next to the registry: result dir holds both (the reaper
	// writes into the same per-task dir). Use the registry's own directory so
	// the check works without a repoRoot argument.
	dir := filepath.Dir(rec.LogPath)
	_, err := os.Stat(filepath.Join(dir, "exit.status"))
	return err == nil
}

// logStats returns the size and the age in seconds of the last modification
// of the worker log (hang detection = log growth, L1-20260912-7).
func logStats(logPath string) (bytes, ageS int64) {
	fi, err := os.Stat(logPath)
	if err != nil {
		return 0, 0
	}
	return fi.Size(), int64(time.Since(fi.ModTime()) / time.Second)
}

// RunningWorkers returns the derived views of every live worker (status
// running). Spawn uses this under the workers lock to enforce parallelism.
func RunningWorkers(repoRoot string) ([]WorkerView, error) {
	views, err := DiscoverWorkers(repoRoot)
	if err != nil {
		return nil, err
	}
	live := make([]WorkerView, 0, len(views))
	for _, v := range views {
		if v.Status == WorkerRunning {
			live = append(live, v)
		}
	}
	return live, nil
}
