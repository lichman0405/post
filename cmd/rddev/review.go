package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/lichman0405/post/internal/devorchestrator"
)

const reviewUsage = `Usage: rddev review <command> TASK [flags]

Commands:
  spawn TASK     dispatch the independent Review Worker for a task in
                 verification: a separate claude process with NO write scope
                 and NO Git control-plane permissions, reviewing the collected
                 diff from inputs in its own result dir
  collect TASK   verify the finished Review Worker: exit 0, no residue, the
                 reviewed code unchanged since spawn (fingerprint), the
                 verdict schema-valid and addressed to the task; records the
                 verdict as merge-gate evidence

The review verdict (approve | request_changes) is recorded under
.rddev/runtime/gates/TASK/ and is required for merge unless the task's gate
override opts out.

Flags:
  --json            machine-readable output
  --tasks-json PATH task DAG file (default tasks/tasks.json)
  --state-json PATH task status file (default tasks/task_status.json)
  --run-id ID       run id (default: generated)
  --model MODEL     claude model override for the Reviewer
  --max-budget-usd N hard USD budget for the Reviewer
  --max-turns N     turn limit for the Reviewer
  --timeout DUR     wall-clock timeout (Go duration, e.g. 30m)
  --claude-bin PATH claude binary to dispatch (default: claude on PATH)
`

func runReview(args []string, stdout, stderr io.Writer, jsonOut bool) int {
	if wantsHelp(args) {
		fmt.Fprint(stdout, reviewUsage)
		return exitOK
	}
	vals, pos, err := parseFlags(args,
		flagSpec{"--json", false},
		flagSpec{"--tasks-json", true},
		flagSpec{"--state-json", true},
		flagSpec{"--run-id", true},
		flagSpec{"--model", true},
		flagSpec{"--max-budget-usd", true},
		flagSpec{"--max-turns", true},
		flagSpec{"--timeout", true},
		flagSpec{"--claude-bin", true},
	)
	if err != nil {
		return usageError(stderr, err.Error(), reviewUsage)
	}
	if _, ok := vals["--json"]; ok {
		jsonOut = true
	}
	if len(pos) == 0 {
		return usageError(stderr, "rddev review: missing subcommand", reviewUsage)
	}
	cmd, taskArg := pos[0], ""
	if len(pos) > 1 {
		taskArg = pos[1]
		if len(pos) > 2 {
			return usageError(stderr, fmt.Sprintf("rddev review %s: unexpected argument %q", cmd, pos[2]), reviewUsage)
		}
	}
	if taskArg == "" {
		return usageError(stderr, fmt.Sprintf("rddev review %s: missing TASK", cmd), reviewUsage)
	}
	repoRoot, err := os.Getwd()
	if err != nil {
		return operationalError(stderr, "rddev review", fmt.Errorf("resolving the repo root: %w", err))
	}

	switch cmd {
	case "spawn":
		opts := &devorchestrator.ReviewSpawnOpts{
			RepoRoot:  repoRoot,
			DagPath:   stringOr(vals["--tasks-json"], devorchestrator.DefaultDAGPath),
			StatePath: stringOr(vals["--state-json"], devorchestrator.DefaultStatePath),
			TaskID:    taskArg,
			Model:     vals["--model"],
			RunID:     vals["--run-id"],
			ClaudeBin: vals["--claude-bin"],
		}
		if v := vals["--max-budget-usd"]; v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f < 0 {
				return usageError(stderr, fmt.Sprintf("rddev review spawn: invalid --max-budget-usd %q", v), reviewUsage)
			}
			opts.MaxBudgetUSD = &f
		}
		if v := vals["--max-turns"]; v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return usageError(stderr, fmt.Sprintf("rddev review spawn: invalid --max-turns %q", v), reviewUsage)
			}
			opts.MaxTurns = &n
		}
		if v := vals["--timeout"]; v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				return usageError(stderr, fmt.Sprintf("rddev review spawn: invalid --timeout %q", v), reviewUsage)
			}
			opts.Timeout = d
		}
		res, err := devorchestrator.SpawnReview(opts)
		if err != nil {
			return operationalError(stderr, "rddev review spawn", err)
		}
		if jsonOut {
			enc := json.NewEncoder(stdout)
			enc.SetEscapeHTML(false)
			enc.Encode(res)
			return exitOK
		}
		fmt.Fprintf(stdout, "%s: spawned Review Worker (run_id=%s, session=%s, pid=%d, claude %s)\n",
			res.TaskID, res.RunID, res.SessionID, res.PID, res.ClaudeVersion)
		fmt.Fprintf(stdout, "  worktree: %s\n  log:      %s\n  registry: %s\n",
			res.Worktree, res.LogPath, res.RegistryPath)
		return exitOK
	case "collect":
		report, err := devorchestrator.CollectReview(&devorchestrator.CollectOpts{
			RepoRoot:  repoRoot,
			DagPath:   stringOr(vals["--tasks-json"], devorchestrator.DefaultDAGPath),
			StatePath: stringOr(vals["--state-json"], devorchestrator.DefaultStatePath),
			TaskID:    taskArg,
			RunID:     vals["--run-id"],
		})
		if err != nil {
			return operationalError(stderr, "rddev review collect", err)
		}
		if jsonOut {
			enc := json.NewEncoder(stdout)
			enc.SetEscapeHTML(false)
			enc.Encode(report)
		} else {
			for _, c := range report.Checks {
				fmt.Fprintf(stdout, "%s\t%s\t%s\n", c.Name, c.Status, c.Detail)
			}
			if report.Verdict != nil {
				fmt.Fprintf(stdout, "%s: review %s — %s\n", report.TaskID, report.Status, report.Verdict.Verdict)
			} else {
				fmt.Fprintf(stdout, "%s: review %s\n", report.TaskID, report.Status)
			}
		}
		if report.Status == "ok" {
			return exitOK
		}
		return exitOperational
	default:
		return usageError(stderr, fmt.Sprintf("rddev review: unknown subcommand %q", cmd), reviewUsage)
	}
}
