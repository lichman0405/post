package devorchestrator

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// `worker spawn` is the real thing the Supervisor did by hand in
// .rddev/dispatch/ for the first ten tasks: create a Supervisor-owned
// worktree, render the task package, generate the guard layer, start a
// genuine independent `claude -p` Worker in it, record the run in the
// registry, and mark the task running. The pipeline is deterministic and
// fails loudly at every step; nothing is silently retried.

const (
	// DefaultParallelWorkers and MaxParallelWorkers bound concurrent Workers
	// (specs/orchestrator/rddev-cli.yaml).
	DefaultParallelWorkers = 3
	MaxParallelWorkers     = 4
)

// SpawnOpts parametrizes one spawn.
type SpawnOpts struct {
	RepoRoot     string
	DagPath      string
	StatePath    string
	TaskID       string
	Model        string
	Effort       string
	MaxBudgetUSD *float64
	MaxTurns     *int
	Timeout      time.Duration
	Docker       bool
	Bare         bool
	Parallel     int // 0 means DefaultParallelWorkers
	RunID        string
	ClaudeBin    string // resolved claude binary; empty means "claude"
}

// SpawnResult reports a successful spawn.
type SpawnResult struct {
	TaskID        string `json:"task_id"`
	RunID         string `json:"run_id"`
	SessionID     string `json:"session_id"`
	PID           int    `json:"pid"`
	Worktree      string `json:"worktree"`
	Branch        string `json:"branch"`
	BaselineSHA   string `json:"baseline_sha"`
	RegistryPath  string `json:"registry_path"`
	LogPath       string `json:"log_path"`
	ClaudeVersion string `json:"claude_version"`
	StartedAt     string `json:"started_at"`
}

// Spawn runs the full spawn pipeline. It owns the ready -> running state
// transition; if that transition fails the just-started Worker is killed, so
// a task is never left running without a running state record.
func Spawn(opts *SpawnOpts) (*SpawnResult, error) {
	repoRoot := opts.RepoRoot
	if repoRoot == "" {
		return nil, fmt.Errorf("spawn requires a repo root")
	}
	if opts.TaskID == "" {
		return nil, fmt.Errorf("spawn requires a task id")
	}
	parallel := opts.Parallel
	if parallel == 0 {
		parallel = DefaultParallelWorkers
	}
	if parallel > MaxParallelWorkers {
		return nil, fmt.Errorf("--parallel %d exceeds the hard maximum of %d (specs/orchestrator/rddev-cli.yaml)", parallel, MaxParallelWorkers)
	}

	store, err := OpenStore(opts.DagPath, opts.StatePath)
	if err != nil {
		return nil, err
	}
	if store.dag.Get(opts.TaskID) == nil {
		return nil, fmt.Errorf("unknown task %s in task DAG", opts.TaskID)
	}

	claudeBin := opts.ClaudeBin
	if claudeBin == "" {
		claudeBin = "claude"
	}
	if _, err := exec.LookPath(claudeBin); err != nil {
		return nil, fmt.Errorf("claude CLI not found on PATH (looked for %q): %w — the Worker is a genuine independent claude -p process, not a subagent", claudeBin, err)
	}

	// 1) Worktree: one per task, Supervisor-owned, never shared. A
	// pre-existing worktree is adopted only if it is a real git worktree; a
	// conflicting plain directory is an error, never deleted.
	worktree := filepath.Join(WorktreesDir(repoRoot), opts.TaskID)
	branch := TaskBranch(opts.TaskID, store.dag.Get(opts.TaskID).Title)
	if err := ensureWorktree(repoRoot, opts.TaskID, branch); err != nil {
		return nil, err
	}

	// 2) Baseline + refs snapshot (L1-20260912-16: refs/remotes/** and
	// refs/heads/task/** are shared Supervisor state, excluded from the
	// collect-time comparison).
	baseline, err := gitOutput(repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("reading baseline HEAD: %w", err)
	}
	refsBefore, err := refsSnapshot(repoRoot)
	if err != nil {
		return nil, err
	}

	// 3) Parallelism gate: count live Workers from disk (never from memory)
	// while holding the workers-dir lock, so two concurrent spawns cannot
	// both pass the gate.
	if err := gateParallelism(repoRoot, parallel); err != nil {
		return nil, err
	}

	// 4) Render the task package and validate it against the schema the repo
	// itself carries (no embedded copy to drift).
	taskSpec := store.dag.Get(opts.TaskID)
	pkg, err := RenderTaskPackage(taskSpec, baseline, opts.MaxTurns, opts.MaxBudgetUSD)
	if err != nil {
		return nil, err
	}
	schemaPath := filepath.Join(repoRoot, "specs", "orchestrator", "task-package.schema.json")
	if err := ValidateTaskPackage(pkg, schemaPath); err != nil {
		return nil, err
	}

	// 5) Write the runtime files: task package, prompts, guard layer.
	taskDir := WorkerTaskDir(repoRoot, opts.TaskID)
	env, err := BuildWorkerEnv(os.Environ(), repoRoot, opts.TaskID, worktree, taskDir, opts.Docker)
	if err != nil {
		return nil, err
	}
	prompt := RenderPrompt(pkg, worktree, taskDir, WorktreesDir(repoRoot), WorkersDir(repoRoot))
	system := RenderSystemPrompt(opts.TaskID, repoRoot, worktree, taskDir, WorktreesDir(repoRoot))
	if err := writeSpawnFiles(taskDir, pkg, prompt, system, GuardOpts{
		RepoRoot: repoRoot, TaskID: opts.TaskID, Worktree: worktree,
		ResultDir: taskDir, DockerGrant: opts.Docker,
	}); err != nil {
		return nil, err
	}

	// 6) Record the claude version the Worker will run with (L1-20260912-5).
	claudeVersion, err := gitOutput2(claudeBin, "--version")
	if err != nil {
		return nil, fmt.Errorf("reading claude version: %w", err)
	}

	// 7) Start the reaper detached; it writes claude.pid, prints the pid,
	// waits for claude and records exit.status — surviving rddev's own exit.
	runID := opts.RunID
	if runID == "" {
		runID = NewRunID()
	}
	sessionID, err := newUUID()
	if err != nil {
		return nil, err
	}
	logPath := filepath.Join(taskDir, "worker.log")
	pidFile := filepath.Join(taskDir, "claude.pid")
	statusFile := filepath.Join(taskDir, "exit.status")
	settingsPath := filepath.Join(taskDir, "worker-settings.json")
	systemPath := filepath.Join(taskDir, "system.md")
	args, err := claudeArgs(opts, sessionID, settingsPath, systemPath, prompt)
	if err != nil {
		return nil, err
	}
	reaper := filepath.Join(taskDir, "run-worker.sh")
	// The reaper runs "$@" after the four path args. Without a timeout the
	// command is `claude <args>`; with one, the wrapper must surround the
	// binary (`timeout ... claude <args>`) so the reaper records 124 on
	// expiry — a timed-out Worker is exited-with-124, never "completed".
	var workerCmd []string
	if opts.Timeout > 0 {
		workerCmd = wrapTimeout(opts.Timeout, claudeBin, args)
	} else {
		workerCmd = append([]string{claudeBin}, args...)
	}
	cmdArgs := append([]string{reaper, worktree, logPath, pidFile, statusFile}, workerCmd...)
	cmd := exec.Command("bash", cmdArgs...)
	cmd.Dir = repoRoot
	cmd.Env = env.Vars
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting the reaper wrapper: %w", err)
	}
	// Read the pid the reaper prints, with a deadline; then detach (Release,
	// never Wait — the reaper outlives rddev by design).
	pidCh := make(chan int, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		if sc.Scan() {
			var pid int
			if _, err := fmt.Sscanf(sc.Text(), "%d", &pid); err == nil {
				pidCh <- pid
			}
		}
		close(pidCh)
	}()
	var workerPID int
	select {
	case workerPID = <-pidCh:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		return nil, fmt.Errorf("the reaper wrapper did not report a pid within 10s — spawn aborted, nothing was recorded")
	}
	if workerPID <= 0 {
		cmd.Process.Kill()
		return nil, fmt.Errorf("the reaper wrapper reported an invalid pid — spawn aborted, nothing was recorded")
	}
	_ = cmd.Process.Release()

	// 8) Record the registry fact (status running; exit info merged later).
	startedAt := time.Now().UTC().Format(time.RFC3339)
	startTime, err := procStartTime(workerPID)
	if err != nil {
		killWorker(workerPID)
		return nil, fmt.Errorf("reading /proc starttime for pid %d: %w", workerPID, err)
	}
	rec := &WorkerRecord{
		TaskID:        opts.TaskID,
		RunID:         runID,
		SessionID:     sessionID,
		ClaudeVersion: claudeVersion,
		Model:         opts.Model,
		Effort:        opts.Effort,
		MaxBudgetUSD:  opts.MaxBudgetUSD,
		MaxTurns:      opts.MaxTurns,
		Timeout:       opts.Timeout.String(),
		DockerGrant:   opts.Docker,
		PID:           workerPID,
		StartTime:     startTime,
		Worktree:      worktree,
		Branch:        branch,
		BaselineSHA:   baseline,
		RefsBefore:    refsBefore,
		LogPath:       logPath,
		ResultDir:     taskDir,
		StartedAt:     startedAt,
	}
	if err := SaveRegistry(repoRoot, rec); err != nil {
		killWorker(workerPID)
		return nil, fmt.Errorf("writing worker registry: %w", err)
	}

	// 9) ready -> running (the spawn command owns this transition,
	// task-state-machine.yaml). If the state file refuses it, kill the Worker
	// and undo nothing else — the registry remains as an honest record of an
	// aborted attempt.
	if _, err := store.StartWorker(opts.TaskID, runID, startedAt); err != nil {
		killWorker(workerPID)
		return nil, fmt.Errorf("worker started but the ready -> running transition failed; the worker was killed: %w", err)
	}

	return &SpawnResult{
		TaskID:        opts.TaskID,
		RunID:         runID,
		SessionID:     sessionID,
		PID:           workerPID,
		Worktree:      worktree,
		Branch:        branch,
		BaselineSHA:   baseline,
		RegistryPath:  registryPath(repoRoot, opts.TaskID),
		LogPath:       logPath,
		ClaudeVersion: claudeVersion,
		StartedAt:     startedAt,
	}, nil
}

// StartWorker applies ready -> running and stamps worker_run_id/started_at
// (the spawn command owns this transition, specs/orchestrator/
// task-state-machine.yaml command_mapping).
func (s *Store) StartWorker(id, runID, startedAt string) (*TransitionResult, error) {
	var result *TransitionResult
	err := s.mutate(id, func(ts *TaskState, from State, states map[string]State) error {
		if err := checkTransition(id, from, StateRunning); err != nil {
			return err
		}
		ts.Status = StateRunning
		ts.History = append(ts.History, StateChange{From: from, To: StateRunning, At: startedAt, RunID: runID, Reason: "worker spawn"})
		ts.WorkerRunID = runID
		ts.StartedAt = strptr(startedAt)
		result = &TransitionResult{TaskID: id, From: from, To: StateRunning, RunID: runID, At: startedAt}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// TaskBranch names the per-task branch (Supervisor namespace refs/heads/task/**,
// excluded from the collect-time refs comparison, L1-20260912-16).
func TaskBranch(taskID, title string) string {
	slug := strings.ToLower(title)
	slug = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r == ' ' || r == '_' || r == '/':
			return '-'
		}
		return -1
	}, slug)
	slug = strings.Trim(slug, "-")
	// collapsed runs of dashes: "A & B" dropped the '&' but kept both spaces
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	if slug == "" {
		slug = "work"
	}
	return "task/" + taskID + "-" + slug
}

// ensureWorktree creates or adopts the task worktree on the task branch.
func ensureWorktree(repoRoot, taskID, branch string) error {
	wtDir := filepath.Join(WorktreesDir(repoRoot), taskID)
	fi, err := os.Stat(wtDir)
	switch {
	case err == nil && fi.IsDir():
		// adopt: verify it is a git worktree on the expected branch
		out, gerr := gitOutput(wtDir, "rev-parse", "--git-dir")
		if gerr != nil || strings.TrimSpace(out) == "" {
			return fmt.Errorf("worktree path %s exists but is not a git worktree (git rev-parse --git-dir failed: %v) — refusing to adopt or delete it", wtDir, gerr)
		}
		b, gerr := gitOutput(wtDir, "branch", "--show-current")
		if gerr != nil {
			return fmt.Errorf("reading worktree branch at %s: %w", wtDir, gerr)
		}
		if strings.TrimSpace(b) != branch {
			return fmt.Errorf("worktree %s exists on branch %q, expected %q — refusing to adopt a worktree from a different attempt", wtDir, strings.TrimSpace(b), branch)
		}
		return nil
	case err == nil:
		return fmt.Errorf("worktree path %s exists but is not a directory — refusing to delete it", wtDir)
	case os.IsNotExist(err):
		// fall through to create
	default:
		return err
	}

	// The branch may already exist from an earlier attempt; create it only if
	// missing (creating branches is Supervisor-owned — rddev does it here on
	// the Supervisor's behalf, exactly like the dispatch harness did).
	exists, err := branchExists(repoRoot, branch)
	if err != nil {
		return err
	}
	if !exists {
		if _, err := gitOutput(repoRoot, "checkout", "-b", branch); err != nil {
			return fmt.Errorf("creating task branch %s: %w", branch, err)
		}
		if _, err := gitOutput(repoRoot, "checkout", "-"); err != nil {
			return fmt.Errorf("returning to the previous branch after creating %s: %w", branch, err)
		}
	}
	if _, err := gitOutput(repoRoot, "worktree", "add", wtDir, branch); err != nil {
		return fmt.Errorf("creating worktree %s for branch %s: %w", wtDir, branch, err)
	}
	return nil
}

func branchExists(repoRoot, branch string) (bool, error) {
	out, err := gitOutput(repoRoot, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		// gitOutput wraps the exec error, so unwrap with errors.As — exit 1
		// means "no such ref", which is the expected answer, not a failure.
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("checking branch %s: %w", branch, err)
	}
	_ = out
	return true, nil
}

// refsSnapshot records every ref except the shared ones (L1-20260912-16).
func refsSnapshot(repoRoot string) ([]string, error) {
	out, err := gitOutput(repoRoot, "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		return nil, fmt.Errorf("snapshotting refs: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	kept := make([]string, 0, len(lines))
	for _, l := range lines {
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, "refs/remotes/") || strings.HasPrefix(l, "refs/heads/task/") {
			continue
		}
		kept = append(kept, l)
	}
	return kept, nil
}

// gateParallelism counts live Workers from disk under the workers-dir lock.
func gateParallelism(repoRoot string, limit int) error {
	if err := os.MkdirAll(WorkersDir(repoRoot), 0o755); err != nil {
		return err
	}
	lock, err := lockPathFor(filepath.Join(WorkersDir(repoRoot), "registry.json"))
	if err != nil {
		return err
	}
	unlock, err := lockFile(lock)
	if err != nil {
		return err
	}
	defer unlock()
	live, err := RunningWorkers(repoRoot)
	if err != nil {
		return err
	}
	if len(live) >= limit {
		return fmt.Errorf("parallelism limit reached: %d Worker(s) running, limit %d (default 3, hard max 4) — retry after one finishes (rddev worker list)", len(live), limit)
	}
	return nil
}

func writeSpawnFiles(taskDir string, pkg *TaskPackage, prompt, system string, guard GuardOpts) error {
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		return err
	}
	pkgJSON, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return err
	}
	files := map[string]struct {
		data []byte
		mode os.FileMode
	}{
		"task-package.json": {append(pkgJSON, '\n'), 0o644},
		"prompt.md":         {[]byte(prompt), 0o644},
		"system.md":         {[]byte(system), 0o644},
	}
	for name, f := range files {
		if err := os.WriteFile(filepath.Join(taskDir, name), f.data, f.mode); err != nil {
			return err
		}
	}
	_, err = WriteGuardFiles(taskDir, guard)
	return err
}

// gitOutput runs git in dir and returns trimmed stdout; on failure the error
// carries the command's stderr.
func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		// %w keeps the *exec.ExitError in the chain so callers can inspect the
		// exit code (branchExists treats exit 1 as "no such branch").
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), ee, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitOutput2 runs bin and returns trimmed stdout (used for claude --version).
func gitOutput2(bin string, args ...string) (string, error) {
	cmd := exec.Command(bin, args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), ee, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// procStartTime reads /proc/<pid>/stat field 22 (clock ticks since boot).
func procStartTime(pid int) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	rest := data
	if i := strings.LastIndexByte(string(rest), ')'); i >= 0 {
		rest = rest[i+1:]
	}
	fields := strings.Fields(string(rest))
	if len(fields) < 20 {
		return 0, fmt.Errorf("/proc/%d/stat has only %d fields after the command", pid, len(fields))
	}
	var st uint64
	if _, err := fmt.Sscanf(fields[19], "%d", &st); err != nil {
		return 0, fmt.Errorf("parsing /proc/%d/stat starttime %q: %w", pid, fields[19], err)
	}
	return st, nil
}

// killWorker terminates the just-spawned worker (SIGTERM then SIGKILL after a
// short grace). Used only when spawn itself fails after the process started —
// a task must never be left running without a state record.
func killWorker(pid int) {
	_ = syscall.Kill(pid, syscall.SIGTERM)
	time.Sleep(2 * time.Second)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// newUUID returns a random RFC 4122 version-4 UUID string.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
