package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lichman0405/post/internal/devorchestrator"
)

const workerUsage = `Usage: rddev worker <command> [TASK] [flags]

Commands:
  spawn TASK   create the Supervisor-owned worktree, render the task package,
               generate the guard layer and start an independent claude -p
               Worker; performs ready -> running
  list         discover every Worker from disk: running / stale / exited, with
               stream-log growth for hang detection
  logs TASK    print the tail of the Worker's stream log
  stop TASK    terminate the Worker (SIGTERM, then SIGKILL after a grace
               period) and record the exit; never marks the task completed
  collect TASK verify the finished run against real state and advance it:
               HEAD == baseline, task branch unmoved, no new refs, diff inside
               allowed_scope, RESULT.json validates worker-result.schema.json,
               no secret material, no host residue; running -> verification /
               rejected / worker_failed. Exit 0 = clean, 1 = rejected/failed.
               The report is also written to the task dir (collect-report.json)

Flags:
  --json                machine-readable output
  --tasks-json PATH     task DAG file (default tasks/tasks.json)
  --state-json PATH     task status file (default tasks/task_status.json)
  --run-id ID           run id stamped on the state change (default: generated)
  --model MODEL         claude model override (default: inherited from the
                        Supervisor environment, L1-20260912-5)
  --effort LEVEL        claude effort override (default: inherited)
  --max-budget-usd N    hard USD budget for this Worker
  --max-turns N         turn limit for this Worker
  --timeout DUR         wall-clock timeout (Go duration, e.g. 45m); a timed-out
                        Worker exits 124 and is recorded failed, not completed
  --docker              per-task opt-in Docker grant (guard + COMPOSE_PROJECT_NAME
                        namespacing)
  --bare                run claude with --bare (skip CLAUDE.md discovery)
  --parallel N          concurrency limit for the gate (default 3, hard max 4)
  --lines N             logs: number of tail lines (default 50)

Parallelism is default 3, hard max 4 (specs/orchestrator/rddev-cli.yaml).
Workers are genuine separate claude processes, one per task worktree — never
subagents, never shared worktrees.
`

func runWorker(args []string, stdout, stderr io.Writer, jsonOut bool) int {
	if wantsHelp(args) {
		fmt.Fprint(stdout, workerUsage)
		return exitOK
	}
	vals, pos, err := parseFlags(args,
		flagSpec{"--json", false},
		flagSpec{"--tasks-json", true},
		flagSpec{"--state-json", true},
		flagSpec{"--run-id", true},
		flagSpec{"--model", true},
		flagSpec{"--effort", true},
		flagSpec{"--max-budget-usd", true},
		flagSpec{"--max-turns", true},
		flagSpec{"--timeout", true},
		flagSpec{"--docker", false},
		flagSpec{"--bare", false},
		flagSpec{"--parallel", true},
		flagSpec{"--lines", true},
	)
	if err != nil {
		return usageError(stderr, err.Error(), workerUsage)
	}
	if _, ok := vals["--json"]; ok {
		jsonOut = true
	}
	if len(pos) == 0 {
		return usageError(stderr, "rddev worker: missing subcommand", workerUsage)
	}
	cmd, taskArg := pos[0], ""
	if len(pos) > 1 {
		taskArg = pos[1]
		if len(pos) > 2 {
			return usageError(stderr, fmt.Sprintf("rddev worker %s: unexpected argument %q", cmd, pos[2]), workerUsage)
		}
	}

	repoRoot, err := os.Getwd()
	if err != nil {
		return operationalError(stderr, "rddev worker", fmt.Errorf("resolving the repo root: %w", err))
	}

	switch cmd {
	case "spawn":
		if taskArg == "" {
			return usageError(stderr, "rddev worker spawn: missing TASK", workerUsage)
		}
		return runWorkerSpawn(vals, taskArg, repoRoot, stdout, stderr, jsonOut)
	case "list":
		if taskArg != "" {
			return usageError(stderr, "rddev worker list: takes no TASK argument", workerUsage)
		}
		return runWorkerList(repoRoot, stdout, stderr, jsonOut)
	case "logs":
		if taskArg == "" {
			return usageError(stderr, "rddev worker logs: missing TASK", workerUsage)
		}
		return runWorkerLogs(repoRoot, taskArg, vals["--lines"], stdout, stderr)
	case "stop":
		if taskArg == "" {
			return usageError(stderr, "rddev worker stop: missing TASK", workerUsage)
		}
		return runWorkerStop(repoRoot, taskArg, stdout, stderr, jsonOut)
	case "collect":
		if taskArg == "" {
			return usageError(stderr, "rddev worker collect: missing TASK", workerUsage)
		}
		return runWorkerCollect(vals, taskArg, repoRoot, stdout, stderr, jsonOut)
	default:
		return usageError(stderr, fmt.Sprintf("rddev worker: unknown subcommand %q", cmd), workerUsage)
	}
}

// runWorkerSpawn parses spawn-specific flags and calls the spawn pipeline.
func runWorkerSpawn(vals map[string]string, taskID, repoRoot string, stdout, stderr io.Writer, jsonOut bool) int {
	opts := &devorchestrator.SpawnOpts{
		RepoRoot:  repoRoot,
		DagPath:   stringOr(vals["--tasks-json"], devorchestrator.DefaultDAGPath),
		StatePath: stringOr(vals["--state-json"], devorchestrator.DefaultStatePath),
		TaskID:    taskID,
		Model:     vals["--model"],
		Effort:    vals["--effort"],
		RunID:     vals["--run-id"],
		Docker:    vals["--docker"] != "",
		Bare:      vals["--bare"] != "",
	}
	if v := vals["--max-budget-usd"]; v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return usageError(stderr, fmt.Sprintf("rddev worker spawn: invalid --max-budget-usd %q", v), workerUsage)
		}
		opts.MaxBudgetUSD = &f
	}
	if v := vals["--max-turns"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return usageError(stderr, fmt.Sprintf("rddev worker spawn: invalid --max-turns %q", v), workerUsage)
		}
		opts.MaxTurns = &n
	}
	if v := vals["--timeout"]; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return usageError(stderr, fmt.Sprintf("rddev worker spawn: invalid --timeout %q", v), workerUsage)
		}
		opts.Timeout = d
	}
	if v := vals["--parallel"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return usageError(stderr, fmt.Sprintf("rddev worker spawn: invalid --parallel %q", v), workerUsage)
		}
		opts.Parallel = n
	}

	res, err := devorchestrator.Spawn(opts)
	if err != nil {
		return operationalError(stderr, "rddev worker spawn", err)
	}
	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		enc.Encode(res)
		return exitOK
	}
	fmt.Fprintf(stdout, "%s: spawned Worker (run_id=%s, session=%s, pid=%d, claude %s)\n",
		res.TaskID, res.RunID, res.SessionID, res.PID, res.ClaudeVersion)
	fmt.Fprintf(stdout, "  worktree: %s (%s @ %s)\n", res.Worktree, res.Branch, res.BaselineSHA)
	fmt.Fprintf(stdout, "  log:      %s\n  registry: %s\n", res.LogPath, res.RegistryPath)
	return exitOK
}

// runWorkerList prints the disk-derived view of every Worker.
func runWorkerList(repoRoot string, stdout, stderr io.Writer, jsonOut bool) int {
	views, err := devorchestrator.DiscoverWorkers(repoRoot)
	if err != nil {
		return operationalError(stderr, "rddev worker list", err)
	}
	if jsonOut {
		type listEntry struct {
			TaskID       string   `json:"task_id"`
			RunID        string   `json:"run_id"`
			Status       string   `json:"status"`
			PID          int      `json:"pid"`
			Model        string   `json:"model"`
			MaxBudgetUSD *float64 `json:"max_budget_usd,omitempty"`
			ExitStatus   *int     `json:"exit_status,omitempty"`
			BaselineSHA  string   `json:"baseline_sha"`
			Worktree     string   `json:"worktree"`
			LogPath      string   `json:"log_path"`
			LogBytes     int64    `json:"log_bytes"`
			LogAgeS      int64    `json:"log_age_s"`
			StartedAt    string   `json:"started_at"`
			EndedAt      string   `json:"ended_at,omitempty"`
		}
		out := make([]listEntry, 0, len(views))
		for _, v := range views {
			out = append(out, listEntry{
				TaskID: v.Record.TaskID, RunID: v.Record.RunID, Status: string(v.Status),
				PID: v.Record.PID, Model: v.Record.Model, MaxBudgetUSD: v.Record.MaxBudgetUSD,
				ExitStatus: v.Record.ExitStatus, BaselineSHA: v.Record.BaselineSHA,
				Worktree: v.Record.Worktree, LogPath: v.Record.LogPath,
				LogBytes: v.LogBytes, LogAgeS: v.LogAgeS,
				StartedAt: v.Record.StartedAt, EndedAt: v.Record.EndedAt,
			})
		}
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		enc.Encode(struct {
			Workers []listEntry `json:"workers"`
		}{Workers: out})
		return exitOK
	}
	if len(views) == 0 {
		fmt.Fprintln(stdout, "no Workers recorded")
		return exitOK
	}
	for _, v := range views {
		line := fmt.Sprintf("%s\t%s\tpid=%d", v.Record.TaskID, v.Status, v.Record.PID)
		if v.Status == devorchestrator.WorkerRunning {
			line += fmt.Sprintf("\tlog=%dB, grew %ds ago", v.LogBytes, v.LogAgeS)
		} else if v.Record.ExitStatus != nil {
			line += fmt.Sprintf("\texit=%d (%s)", *v.Record.ExitStatus, v.Record.ExitSource)
		} else {
			line += "\tno exit status recorded"
		}
		if v.Record.Model != "" {
			line += "\tmodel=" + v.Record.Model
		}
		fmt.Fprintln(stdout, line)
	}
	return exitOK
}

// runWorkerLogs prints the tail of the Worker stream log.
func runWorkerLogs(repoRoot, taskID, linesVal string, stdout, stderr io.Writer) int {
	lines := 50
	if linesVal != "" {
		n, err := strconv.Atoi(linesVal)
		if err != nil || n < 1 {
			return usageError(stderr, fmt.Sprintf("rddev worker logs: invalid --lines %q", linesVal), workerUsage)
		}
		lines = n
	}
	rec, err := devorchestrator.LoadRegistry(repoRoot, taskID)
	if err != nil {
		return operationalError(stderr, "rddev worker logs", err)
	}
	if rec == nil {
		return operationalError(stderr, "rddev worker logs", fmt.Errorf("no recorded Worker for %s", taskID))
	}
	data, err := os.ReadFile(rec.LogPath)
	if err != nil {
		return operationalError(stderr, "rddev worker logs", fmt.Errorf("reading log %s: %w", rec.LogPath, err))
	}
	all := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
		fmt.Fprintf(stdout, "… %s (last %d lines)\n", rec.LogPath, lines)
	} else {
		fmt.Fprintf(stdout, "… %s (%d lines)\n", rec.LogPath, len(all))
	}
	for _, l := range all {
		fmt.Fprintln(stdout, l)
	}
	return exitOK
}

// runWorkerStop terminates the Worker and records the exit. It never marks
// the task completed — collect (T0011) owns that judgement; a stopped Worker
// reads as exited-with-signal and the task stays running until the
// Supervisor collects it.
func runWorkerStop(repoRoot, taskID string, stdout, stderr io.Writer, jsonOut bool) int {
	rec, err := devorchestrator.LoadRegistry(repoRoot, taskID)
	if err != nil {
		return operationalError(stderr, "rddev worker stop", err)
	}
	if rec == nil {
		return operationalError(stderr, "rddev worker stop", fmt.Errorf("no recorded Worker for %s", taskID))
	}
	if rec.ExitStatus != nil {
		fmt.Fprintf(stdout, "%s: Worker already exited (exit %d, %s)\n", taskID, *rec.ExitStatus, rec.ExitSource)
		return exitOK
	}
	// Never signal a recycled pid: the recorded starttime must still match.
	// stopWorker does the same check right before killing.
	if err := devorchestrator.StopWorker(repoRoot, rec); err != nil {
		return operationalError(stderr, "rddev worker stop", err)
	}
	// Re-derive from disk so the recorded exit is reported, not guessed.
	views, err := devorchestrator.DiscoverWorkers(repoRoot)
	if err != nil {
		return operationalError(stderr, "rddev worker stop", err)
	}
	for _, v := range views {
		if v.Record.TaskID != taskID {
			continue
		}
		if jsonOut {
			enc := json.NewEncoder(stdout)
			enc.SetEscapeHTML(false)
			enc.Encode(struct {
				TaskID     string `json:"task_id"`
				Status     string `json:"status"`
				ExitStatus *int   `json:"exit_status,omitempty"`
			}{TaskID: taskID, Status: string(v.Status), ExitStatus: v.Record.ExitStatus})
			return exitOK
		}
		if v.Record.ExitStatus != nil {
			fmt.Fprintf(stdout, "%s: stopped, exit %d recorded (%s)\n", taskID, *v.Record.ExitStatus, v.Record.ExitSource)
		} else {
			fmt.Fprintf(stdout, "%s: stopped, status %s (no exit status recorded)\n", taskID, v.Status)
		}
		return exitOK
	}
	fmt.Fprintf(stdout, "%s: stopped\n", taskID)
	return exitOK
}
