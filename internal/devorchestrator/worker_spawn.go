package devorchestrator

import (
	"bufio"
	"context"
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
	// FromState is the state the spawn transitions from: ready for a first
	// dispatch, rejected for a respawn (T0012 — a rejected task re-dispatching
	// a Worker never detours through ready). Empty means ready.
	FromState State
	// ResumeSession resumes an existing claude session instead of starting a
	// new one (rework: the same Worker continues with its context intact).
	ResumeSession string
	// ReworkReason is appended to the prompt (the rejection evidence the
	// Worker must address).
	ReworkReason string
	// ResetWorktree discards the worktree's uncommitted diff before dispatch
	// (respawn only — the rejected diff's evidence is already recorded).
	ResetWorktree bool
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
	taskSpec := store.dag.Get(opts.TaskID)
	worktree := filepath.Join(WorktreesDir(repoRoot), opts.TaskID)
	branch := TaskBranch(opts.TaskID, taskSpec.Title)
	if err := ensureWorktree(repoRoot, opts.TaskID, branch); err != nil {
		return nil, err
	}

	// 1b) A respawn starts from a clean tree: the rejected diff is discarded
	// (its evidence lives in the collect report and the reject record) and
	// the worktree is reset to the branch tip — the new Worker starts from
	// the baseline, never from a rejected attempt's leftovers.
	if opts.ResetWorktree {
		if _, err := gitOutput(worktree, "reset", "--hard", "HEAD"); err != nil {
			return nil, fmt.Errorf("resetting the rejected worktree for a respawn: %w", err)
		}
		if _, err := gitOutput(worktree, "clean", "-fd"); err != nil {
			return nil, fmt.Errorf("cleaning the rejected worktree for a respawn: %w", err)
		}
	}

	// 2) Baseline + refs snapshot (L1-20260912-16: refs/remotes/** and
	// refs/heads/task/** are shared Supervisor state, excluded from the
	// collect-time comparison). The baseline is the TASK BRANCH's head — the
	// repo-root HEAD is the Supervisor's working branch, not the code the
	// Worker starts from.
	baseline, err := gitOutput(repoRoot, "rev-parse", "--verify", "refs/heads/"+branch)
	if err != nil {
		return nil, fmt.Errorf("reading baseline of refs/heads/%s: %w", branch, err)
	}
	// Record the branch in the Supervisor's ref ledger BEFORE the Worker
	// exists, because a sibling Worker running right now will see this ref
	// appear: collect exempts a new ref only when the Supervisor recorded
	// creating it (ref_ledger.go), and this is where the Supervisor does that.
	if err := RecordSupervisorRef(repoRoot, "refs/heads/"+branch, baseline, "spawn", opts.TaskID); err != nil {
		return nil, fmt.Errorf("recording refs/heads/%s in the Supervisor ref ledger: %w", branch, err)
	}
	refsBefore, err := refsSnapshot(repoRoot)
	if err != nil {
		return nil, err
	}

	// 2b) Pre-dispatch scope validation (T0012 requirement 3): the
	// allowed_scope is checked against the real tree before dispatch — a
	// plain entry matching nothing is a hard error, a dead /** entry a
	// warning, and a scope covering a derived artifact's marker inputs must
	// also cover the derived artifact.
	scopeV, err := ValidateScopeAgainstTree(taskSpec.AllowedScope, repoRoot, derivedAt(repoRoot))
	if err != nil {
		return nil, fmt.Errorf("validating allowed_scope before dispatch: %w", err)
	}
	if len(scopeV.HardErrors) > 0 {
		return nil, fmt.Errorf("allowed_scope of %s does not validate against the real tree: %s", opts.TaskID, strings.Join(scopeV.HardErrors, "; "))
	}

	// 3) Parallelism gate: count live Workers from disk (never from memory)
	// while holding the workers-dir lock, so two concurrent spawns cannot
	// both pass the gate.
	if err := gateParallelism(repoRoot, parallel); err != nil {
		return nil, err
	}

	// 4) Render the task package and validate it against the schema the repo
	// itself carries (no embedded copy to drift).
	// A phase must have a real-services gate before any of its tasks run.
	//
	// G3 was vacuous for all 132 tasks because task_overrides started empty,
	// and nothing noticed: every task was simply accepted with G3 recorded as
	// not_required. That is how an entire phase ships with no integration gate,
	// and it is invisible in every individual task's evidence. Refusing the
	// dispatch is the only place the default can be made impossible.
	if err := requirePhaseG3Coverage(repoRoot, DefaultGatesPath, DefaultDAGPath, taskSpec); err != nil {
		return nil, err
	}
	pkg, err := RenderTaskPackage(taskSpec, baseline, opts.MaxTurns, opts.MaxBudgetUSD)
	if err != nil {
		return nil, err
	}
	// Reserve this task's migration number before the Worker exists, so two
	// parallel Workers cannot pick the same one. Idempotent across rework and
	// respawn: a task keeps the number it was contracted with.
	migrationNumber, err := AllocateMigrationNumber(repoRoot, opts.TaskID)
	if err != nil {
		return nil, err
	}
	pkg.MigrationNumber = migrationNumber
	schemaPath := filepath.Join(repoRoot, "specs", "orchestrator", "task-package.schema.json")
	if err := ValidateTaskPackage(pkg, schemaPath); err != nil {
		return nil, err
	}

	// 5) Write the runtime files: task package, prompts, guard layer. A
	// rework appends the rejection evidence to the prompt — the Worker must
	// address the recorded reasons, not re-read them from memory.
	taskDir := WorkerTaskDir(repoRoot, opts.TaskID)
	env, err := BuildWorkerEnv(os.Environ(), repoRoot, opts.TaskID, worktree, taskDir, opts.Docker)
	if err != nil {
		return nil, err
	}
	prompt := RenderPrompt(pkg, worktree, taskDir, WorktreesDir(repoRoot), WorkersDir(repoRoot))
	if opts.ReworkReason != "" {
		prompt += fmt.Sprintf("\n## Rework\n\nThe previous attempt was REJECTED. Fix these recorded reasons, then re-verify:\n\n%s\n", opts.ReworkReason)
	}
	system := RenderSystemPrompt(opts.TaskID, repoRoot, worktree, taskDir, WorktreesDir(repoRoot))
	if err := writeSpawnFiles(taskDir, pkg, prompt, system, GuardOpts{
		RepoRoot: repoRoot, TaskID: opts.TaskID, Worktree: worktree,
		ResultDir: taskDir, DockerGrant: opts.Docker,
	}); err != nil {
		return nil, err
	}

	// 5b) Record the AUTHORITATIVE gate inputs before the Worker starts
	// (T0012 security fix): collect judges by these spawn-time values — never
	// by the copies in the Worker-writable task dir. Byte copies of the
	// package and guard layer are stored alongside, so any Worker edit of its
	// own gate inputs is detected as a tamper, not accepted as a judgement
	// input.
	runID := opts.RunID
	if runID == "" {
		runID = NewRunID()
	}
	// A rework resumes the SAME claude session (context survives); a fresh
	// spawn creates a new one.
	sessionID := opts.ResumeSession
	if sessionID == "" {
		sessionID, err = newUUID()
		if err != nil {
			return nil, err
		}
	}
	// Rework attempts get their own log file — the evidence trail keeps every
	// attempt's log, not just the last one. Computed BEFORE the authoritative
	// gate-inputs record so gate-inputs.json and the registry agree on the
	// same path (a stale worker.log here read as a registry tamper on rework).
	logPath := filepath.Join(taskDir, "worker.log")
	if opts.ReworkReason != "" {
		logPath = filepath.Join(taskDir, "worker-"+runID+".log")
	}
	gateInputs := &GateInputs{
		TaskID:             opts.TaskID,
		RunID:              runID,
		SessionID:          sessionID,
		BaselineSHA:        baseline,
		Branch:             branch,
		Worktree:           worktree,
		ResultDir:          taskDir,
		LogPath:            logPath,
		RefsBefore:         refsBefore,
		AllowedScope:       pkg.AllowedScope,
		RequiredTests:      pkg.RequiredTests,
		AcceptanceCriteria: pkg.AcceptanceCriteria,
	}
	if err := WriteGateInputs(repoRoot, opts.TaskID, gateInputs, taskDir); err != nil {
		return nil, fmt.Errorf("recording authoritative gate inputs before dispatch: %w", err)
	}

	// 6) Record the claude version the Worker will run with (L1-20260912-5),
	// and derive the --json-schema argument from the repo's own
	// worker-result.schema.json (T0011: the RESULT contract is enforced at
	// spawn, not merely requested in prose).
	claudeVersion, err := gitOutput2(claudeBin, "--version")
	if err != nil {
		return nil, fmt.Errorf("reading claude version: %w", err)
	}
	resultSchema, err := resultSchemaForCLI(repoRoot)
	if err != nil {
		return nil, err
	}

	// 7) Start the reaper detached; it writes claude.pid, prints the pid,
	// waits for claude and records exit.status in BOTH the task dir and the
	// Supervisor-owned authoritative runtime dir — surviving rddev's own exit.
	// The run marker is the residue-attribution signal collect reads back
	// from /proc/<pid>/environ (T0011 Defect 2): real claude's Bash tool
	// runs each command in its own session, so a nohup'd survivor escapes
	// the reaper's session — the session scan alone missed it (live probe:
	// sleep 300 alive with PPID 1, not in the reaper's session). The marker
	// travels into every descendant's environment unless the descendant
	// deliberately scrubs it, which is exactly the attribution boundary.
	env.Vars = append(env.Vars, "POST_WORKER_RUN_ID="+runID)
	pidFile := filepath.Join(taskDir, "claude.pid")
	statusFile := filepath.Join(taskDir, "exit.status")
	// A re-dispatch (rework/respawn) must not leave the previous attempt's
	// exit.status behind: observers would read the old attempt's code as the
	// new attempt's (the rework e2e caught a reconcile merging attempt 1's
	// stale authoritative 0 while attempt 2's reaper was mid-write). Both
	// copies are removed; the reaper rewrites them when THIS attempt ends.
	for _, f := range []string{statusFile, authoritativeExitStatusPath(repoRoot, opts.TaskID)} {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("removing the previous attempt's exit status %s: %w", f, err)
		}
	}
	settingsPath := filepath.Join(taskDir, "worker-settings.json")
	systemPath := filepath.Join(taskDir, "system.md")
	args, err := claudeArgs(opts, sessionID, settingsPath, systemPath, prompt, resultSchema)
	if err != nil {
		return nil, err
	}
	reaper := filepath.Join(taskDir, "run-worker.sh")
	// The reaper runs "$@" after the five path args (arg 5 is the
	// authoritative exit.status copy, T0012). Without a timeout the command
	// is `claude <args>`; with one, the wrapper must surround the binary
	// (`timeout ... claude <args>`) so the reaper records 124 on expiry — a
	// timed-out Worker is exited-with-124, never "completed".
	var workerCmd []string
	if opts.Timeout > 0 {
		workerCmd = wrapTimeout(opts.Timeout, claudeBin, args)
	} else {
		workerCmd = append([]string{claudeBin}, args...)
	}
	cmdArgs := append([]string{reaper, worktree, logPath, pidFile, statusFile, authoritativeExitStatusPath(repoRoot, opts.TaskID)}, workerCmd...)
	cmd := exec.Command("bash", cmdArgs...)
	cmd.Dir = repoRoot
	cmd.Env = env.Vars
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// The listening-socket baseline is taken before the reaper starts: every
	// listener that appears later is a candidate for Worker-started residue.
	listenersBefore, err := ListenerSnapshot()
	if err != nil {
		return nil, fmt.Errorf("snapshotting listening sockets for residue detection: %w", err)
	}
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
	// The reaper is the session leader (setsid): every process the Worker
	// spawns inherits this session id, so it is the residue anchor collect
	// scans for. Captured BEFORE Release (Release resets Pid to -1).
	sessionLeaderPID := cmd.Process.Pid
	_ = cmd.Process.Release()

	// Post-spawn environment assertion (T0011 Defect 1): the actual
	// environments of the just-started Worker and its reaper wrapper must
	// carry none of the stripped credential variables. --setting-sources
	// project stops user settings re-injecting them; this check makes spawn
	// fail loudly if one is present in the real processes anyway. A spawn
	// must never silently hand a credential to a Worker.
	for _, pid := range []int{workerPID, sessionLeaderPID} {
		environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		if err != nil {
			killWorker(workerPID)
			return nil, fmt.Errorf("post-spawn environment assertion: reading /proc/%d/environ: %w — the Worker environment could not be verified, spawn aborted and the Worker killed", pid, err)
		}
		if leak := assertCleanWorkerEnv(environ); leak != "" {
			killWorker(workerPID)
			return nil, fmt.Errorf("post-spawn environment assertion: %s is present in the environment of pid %d — the Worker must not hold remote credentials, spawn aborted and the Worker killed", leak, pid)
		}
	}

	// 8) Record the registry fact (status running; exit info merged later).
	startedAt := runStartedAt()
	startTime, err := procStartTime(workerPID)
	if err != nil {
		killWorker(workerPID)
		return nil, fmt.Errorf("reading /proc starttime for pid %d: %w", workerPID, err)
	}
	// Finalize the authoritative gate inputs with the process identity the
	// early WriteGateInputs could not know (the Worker exists now); collect
	// compares the registry against these values, so a Worker-edited
	// registry.json (changed pid/start time/listener baseline) is a detected
	// tamper, not a judged-on input.
	if err := FinalizeGateInputs(repoRoot, opts.TaskID, runID, sessionID, workerPID, startTime, sessionLeaderPID, startedAt, listenersBefore); err != nil {
		killWorker(workerPID)
		return nil, fmt.Errorf("finalizing the authoritative gate inputs: %w", err)
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
		// residue anchors: the Worker's session and the spawn-time listener
		// baseline (T0011, specs/orchestrator/worker-permissions.yaml)
		SessionLeaderPID: sessionLeaderPID,
		ListenersBefore:  listenersBefore,
	}
	if err := SaveRegistry(repoRoot, rec); err != nil {
		killWorker(workerPID)
		return nil, fmt.Errorf("writing worker registry: %w", err)
	}

	// 9) ready -> running, or rejected -> running for a rework/respawn (the
	// spawn command owns these transitions, task-state-machine.yaml). If the
	// state file refuses it, kill the Worker and undo nothing else — the
	// registry remains as an honest record of an aborted attempt.
	from := StateReady
	if opts.FromState != "" {
		from = opts.FromState
	}
	if _, err := store.StartWorkerFrom(opts.TaskID, runID, startedAt, from); err != nil {
		killWorker(workerPID)
		return nil, fmt.Errorf("worker started but the %s -> running transition failed; the worker was killed: %w", from, err)
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
	return s.StartWorkerFrom(id, runID, startedAt, StateReady)
}

// StartWorkerFrom applies from -> running (ready for a first dispatch,
// rejected for a T0012 rework/respawn) and stamps worker_run_id/started_at.
//
// The stamp written into the TASK STATE is rendered into that file's own
// format — ISO 8601 to the second — and not at the precision the caller
// passed in. `tasks/task_status.json` is validated in CI by
// scripts/validate_task_state.py, whose ISO_TS_RE admits no fractional seconds
// and is applied to started_at, completed_at and merged_at (see taskStateTime
// for what that check does and does not read), while the run start spawn hands
// this function carries nanoseconds: a start to the second cannot order a
// verdict written in the same second, which is the defect that precision exists
// for. Both have to hold, and they hold in different files — the registry record
// and the gate inputs keep the full-precision start (and collect compares THOSE
// two against each other), the task state keeps the shape the validator accepts.
// Reverting this leaves the state file unusable to CI on the next spawn; the
// round-1 review measured exactly that (the validator exits 1 on a ns-shaped
// started_at). Pinned by
// TestStartWorkerFromStampsTheTaskStateInItsValidatedFormat.
func (s *Store) StartWorkerFrom(id, runID, startedAt string, from State) (*TransitionResult, error) {
	if s.dag.Get(id) == nil {
		return nil, fmt.Errorf("unknown task %s in task DAG", id)
	}
	at := taskStateStamp(startedAt)
	var result *TransitionResult
	err := s.mutate(id, func(ts *TaskState, fromSt State, states map[string]State) error {
		if fromSt != from {
			return &IllegalTransitionError{ID: id, From: fromSt, To: StateRunning}
		}
		if err := checkTransition(id, fromSt, StateRunning); err != nil {
			return err
		}
		// The dependency rule binds here, not only in Transition: this function
		// is the whole of the start path (`rddev worker spawn`, and rework and
		// respawn through it), and the check wired to Transition in #150 is
		// reached by none of them. Before the transition is applied and before
		// the run is stamped, so a refused start leaves the task exactly where it
		// was — including not having recorded a worker_run_id for a Worker that
		// was already killed by the caller.
		if err := s.requireDepsMerged(id, states); err != nil {
			return err
		}
		ts.Status = StateRunning
		// Both halves survive the merge: `at` is #105's rendering into the task
		// state's own format (the one scripts/validate_task_state.py accepts),
		// and persistedReason is #128's choke point — a reason is redacted as
		// text before it is persisted, not replaced wholesale.
		ts.History = append(ts.History, StateChange{From: fromSt, To: StateRunning, At: at, RunID: runID, Reason: persistedReason("worker spawn")})
		ts.WorkerRunID = runID
		ts.StartedAt = strptr(at)
		result = &TransitionResult{TaskID: id, From: fromSt, To: StateRunning, RunID: runID, At: at}
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
		// The baseline is what the integration branch is NOW — fetched, and
		// named explicitly. Never the checkout's current HEAD, which is a shared
		// resource (reviews, merges, one-off probes) and may be on anything: a
		// task cut from an unrelated checkout inherits commits nobody merged,
		// and a task cut from a stale main starts without the dependency the DAG
		// has just called merged (#123).
		base, err := IntegrationTip(repoRoot)
		if err != nil {
			return err
		}
		// `git branch` rather than `checkout -b`: creating the branch must not
		// move the shared checkout, which is how the checkout could be left
		// sitting on a task branch (L1-20260914-1).
		if _, err := gitOutput(repoRoot, "branch", branch, base); err != nil {
			return fmt.Errorf("creating task branch %s from %s: %w", branch, base, err)
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
		// refs/remotes/** is shared Supervisor state (fetches, other clones).
		// refs/heads/task/** is INCLUDED: it is the namespace a Worker would
		// plausibly reach for, and the task's own branch is already in the
		// snapshot because it is created before this runs.
		if strings.HasPrefix(l, "refs/remotes/") {
			continue
		}
		kept = append(kept, l)
	}
	return kept, nil
}

// parallelismLimit is the text gateParallelism refuses with when the pool is
// full. `rddev` runs as a CHILD of its caller (o.run execs the binary), so the
// driver sees only a combined output string and classifies by this text. The
// literal lives here, beside the refusal that produces it, for the same reason
// the check sentinels live in git_control.go: two literals in two files is how
// they drifted, and that drift was #139.
const parallelismLimit = "parallelism limit reached"

// parallelismLimitRefused reports whether a spawn refusal is the transient
// capacity one rather than a failure to judge.
//
// It is a WAIT and not a decision because waiting ends it: a running Worker
// exits and frees a slot on its own, with nothing for the Supervisor to
// decide. Recorded as a decision it does not end the wait — driver_run.go
// skips any task with an open decision ("retrying it every tick would
// re-record the same decision forever"), and a decision with no run id is
// never cleared. A momentary condition would therefore leave the task stuck
// forever, silently, which is the one outcome §8.2 forbids.
//
// This is L1-20260913-17's rule ("a condition only the driver could fix is not
// a decision") applied to the capacity gate, and the same shape as
// ciStillRunning (#139): an absence is only a wait when waiting can end it.
//
// Three call sites produce a spawn refusal — dispatch and both review-spawn
// paths in stepVerification — and all three go through the same gate.
func parallelismLimitRefused(refusal string) bool {
	return strings.Contains(refusal, parallelismLimit)
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
		return fmt.Errorf("%s: %d Worker(s) running, limit %d (default 3, hard max 4) — retry after one finishes (rddev worker list)", parallelismLimit, len(live), limit)
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
	return runGit(dir, 0, args...)
}

// runGitWaitDelay bounds how long Wait keeps reading a command's output after
// the process itself is gone.
//
// A deadline alone is not a bound, which is the whole reason this exists: git
// spawns helpers (`git-remote-http`, `git-remote-ext`), those helpers inherit
// git's stdout and stderr, and `cmd.Output()` reads until the WRITE END of the
// pipe closes — which the helper holds. Killing git on the deadline therefore
// leaves Wait blocked on a pipe an orphan is still holding, and the fetch returns
// at the deadline plus however long the orphan lives. Measured with an
// `ext::sleep 30` remote and a 300ms deadline: without this, the call did not
// return until the sleep exited. WaitDelay makes Go close the pipes itself once
// it has elapsed, so the deadline is the deadline.
const runGitWaitDelay = 5 * time.Second

// runGit runs git in dir. A positive timeout bounds the call, which matters for
// the one git command this package makes over the network: an unbounded fetch
// does not fail a dispatch, it STALLS it — and under `rddev drive` a dispatch
// that never returns is the whole DAG not moving, with nothing in the log to say
// why. A deadline turns that into an error the driver can retry.
//
// WaitDelay is set even when there is no deadline: a local git that exits while a
// helper holds the pipe hangs the caller exactly the same way, and every caller
// here is on a path where hanging is worse than an error.
func runGit(dir string, timeout time.Duration, args ...string) (string, error) {
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.WaitDelay = runGitWaitDelay
	out, err := cmd.Output()
	if err != nil {
		if timeout > 0 && ctx.Err() != nil {
			return "", fmt.Errorf("git %s: timed out after %s", strings.Join(args, " "), timeout)
		}
		// %w keeps the *exec.ExitError in the chain so callers can inspect the
		// exit code (branchExists treats exit 1 as "no such branch").
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), ee, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitOutputRaw is gitOutput without the trim, for output whose whitespace is
// data — a patch above all.
//
// Trimming a diff is not cosmetic. `git diff` writes an empty context line as a
// single space, and the last line of a diff is very often one; TrimSpace deletes
// it and the final hunk is then one line short of the count in its own @@ header.
// `git apply` reads that as a malformed patch and dies with "corrupt patch at
// line N" — on a change that applies perfectly. The corruption is
// content-dependent (the task's diff has to end on a blank or trailing-space
// line), so it surfaces as an intermittent "does not apply to current main" for
// a branch that is merely behind, and G2 never runs.
func gitOutputRaw(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), ee, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
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

// requireRejectedAndExited gates rework/respawn: the task must be rejected
// and the previous Worker must have exited. A restart of an accepted or
// running task is refused — the state machine is the truth, not the caller's
// intention.
func requireRejectedAndExited(opts *SpawnOpts) error {
	store, err := OpenStore(opts.DagPath, opts.StatePath)
	if err != nil {
		return err
	}
	insp, err := store.Inspect(opts.TaskID)
	if err != nil {
		return err
	}
	if insp.State.Status != StateRejected {
		return fmt.Errorf("task %s is %s, not rejected — rework/respawn re-dispatch a rejected task only (an accepted or running task must never be restarted)", opts.TaskID, insp.State.Status)
	}
	// Reconcile the exit status from disk first (the reaper may have recorded
	// it after the last discovery).
	if _, err := DiscoverWorkers(opts.RepoRoot); err != nil {
		return err
	}
	rec, err := LoadRegistry(opts.RepoRoot, opts.TaskID)
	if err != nil {
		return err
	}
	if rec == nil || rec.ExitStatus == nil {
		return fmt.Errorf("the previous Worker for %s has not exited (or was never recorded) — rework/respawn requires the previous attempt to be over", opts.TaskID)
	}
	return nil
}

// reworkReason renders the rejection evidence the rework prompt carries: the
// newest RejectRecord's reasons (with evidence paths), falling back to the
// state file's rejection reason.
func reworkReason(opts *SpawnOpts) string {
	if rej, ok, err := LatestRecord[RejectRecord](opts.RepoRoot, opts.TaskID, RecordReject); err == nil && ok {
		var b strings.Builder
		for _, r := range rej.Reasons {
			fmt.Fprintf(&b, "- %s\n", r)
		}
		if len(rej.Evidence) > 0 {
			fmt.Fprintf(&b, "\nEvidence: %s\n", strings.Join(rej.Evidence, ", "))
		}
		return b.String()
	}
	store, err := OpenStore(opts.DagPath, opts.StatePath)
	if err != nil {
		return ""
	}
	if insp, err := store.Inspect(opts.TaskID); err == nil && insp.State.RejectionReason != "" {
		return insp.State.RejectionReason
	}
	return "the recorded rejection reasons were not found on disk — inspect the collect report and gate records"
}

// ReworkWorker re-dispatches the SAME Worker on a rejected task (first
// rejection policy): the session is resumed (context intact), the worktree
// keeps its accumulated diff, the prompt carries the rejection evidence, and
// the state moves rejected -> running.
func ReworkWorker(opts *SpawnOpts) (*SpawnResult, error) {
	if err := requireRejectedAndExited(opts); err != nil {
		return nil, err
	}
	rec, err := LoadRegistry(opts.RepoRoot, opts.TaskID)
	if err != nil || rec == nil {
		return nil, fmt.Errorf("loading the previous Worker record for the rework: %w", err)
	}
	opts.ResumeSession = rec.SessionID
	opts.ReworkReason = reworkReason(opts)
	opts.FromState = StateRejected
	opts.ResetWorktree = false
	return Spawn(opts)
}

// RespawnWorker re-dispatches a FRESH Worker on a rejected task (second
// rejection or architecture mismatch policy): a new session, a clean
// worktree reset to the baseline, the rejection evidence in the prompt, and
// rejected -> running.
func RespawnWorker(opts *SpawnOpts) (*SpawnResult, error) {
	if err := requireRejectedAndExited(opts); err != nil {
		return nil, err
	}
	opts.ResumeSession = ""
	opts.ReworkReason = reworkReason(opts)
	opts.FromState = StateRejected
	opts.ResetWorktree = true
	return Spawn(opts)
}

// requirePhaseG3Coverage refuses to dispatch a task whose phase has no G3 job
// anywhere in it.
//
// Not "this task has no G3" — some tasks legitimately need none — but "this
// PHASE has none", which means every task in it would be accepted against
// nothing but its own mocks. The check is deliberately about the phase because
// that is the unit a boundary is drawn at: the checkpoint before P2/P3 wired
// real RSG and real Gitea gates precisely so that "there is no G3 job yet"
// could not be the default they were developed under.
func requirePhaseG3Coverage(repoRoot, gatesPath, dagPath string, task *TaskSpec) error {
	if task == nil {
		return nil
	}
	spec, err := gateSpecAt(repoRoot, gatesPath)
	if err != nil {
		// No gate spec at all: a scratch repository, not a phase under
		// development. Nothing to enforce here — and every other gate refuses
		// without a spec anyway, so this cannot become a way past one.
		// errors.Is, not os.IsNotExist: gateSpecAt wraps the read error, and
		// the unwrapping form is the one that sees through the wrap.
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	dag, err := LoadDAG(dagPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, other := range dag.Tasks {
		if other.Phase != task.Phase {
			continue
		}
		if jobs, err := spec.JobsForGate("G3", other.ID); err == nil && len(jobs) > 0 {
			return nil
		}
	}
	return fmt.Errorf("phase %s has no G3 job for any of its tasks: dispatching %s would develop the whole phase against mocks, with every task accepted against G3=not_required. Wire the phase's real-services gate first (specs/orchestrator/gates.json task_overrides)", task.Phase, task.ID)
}
