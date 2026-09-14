package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/lichman0405/post/internal/devorchestrator"
)

const gitUsage = `Usage: rddev git <command> TASK [flags]

Commands:
  commit TASK    commit the collected worktree diff on the task branch
  push TASK      push the task branch to origin

The Git control plane is Supervisor-only (docs/61 §3): these commands run the
four-gate assertion first and REFUSE — before git is ever invoked — while any
gate is red or missing (G2 must cover every required CI job green, the collect
must be ok and not newer than the G2 evidence, G3 and the review verdict where
required). Every executed action is recorded under .rddev/runtime/gates/TASK/.

Flags:
  --json            machine-readable output
  --tasks-json PATH task DAG file (default tasks/tasks.json)
  --state-json PATH task status file (default tasks/task_status.json)
  --gates PATH      gate spec file (default specs/orchestrator/gates.json)
  --run-id ID       run id stamped on the git record (default: generated)
  --message TEXT    commit message (default: rendered from the task DAG)
`

// runGit replaces the T0012 stub: real commit/push with the gate assertion.
func runGit(args []string, stdout, stderr io.Writer, jsonOut bool) int {
	if wantsHelp(args) {
		fmt.Fprint(stdout, gitUsage)
		return exitOK
	}
	vals, pos, err := parseFlags(args,
		flagSpec{"--json", false},
		flagSpec{"--tasks-json", true},
		flagSpec{"--state-json", true},
		flagSpec{"--gates", true},
		flagSpec{"--run-id", true},
		flagSpec{"--message", true},
	)
	if err != nil {
		return usageError(stderr, err.Error(), gitUsage)
	}
	if _, ok := vals["--json"]; ok {
		jsonOut = true
	}
	if len(pos) == 0 {
		return usageError(stderr, "rddev git: missing subcommand", gitUsage)
	}
	cmd, taskArg := pos[0], ""
	if len(pos) > 1 {
		taskArg = pos[1]
		if len(pos) > 2 {
			return usageError(stderr, fmt.Sprintf("rddev git %s: unexpected argument %q", cmd, pos[2]), gitUsage)
		}
	}
	if taskArg == "" {
		return usageError(stderr, fmt.Sprintf("rddev git %s: missing TASK", cmd), gitUsage)
	}
	repoRoot, err := os.Getwd()
	if err != nil {
		return operationalError(stderr, "rddev git", fmt.Errorf("resolving the repo root: %w", err))
	}
	action := map[string]string{"commit": "commit", "push": "push"}[cmd]
	if action == "" {
		return usageError(stderr, fmt.Sprintf("rddev git: unknown subcommand %q (valid: commit, push)", cmd), gitUsage)
	}
	opts := &devorchestrator.GitControlOpts{
		RepoRoot:  repoRoot,
		GatesPath: vals["--gates"],
		TaskID:    taskArg,
		RunID:     vals["--run-id"],
		CommitMsg: vals["--message"],
		DagPath:   stringOr(vals["--tasks-json"], devorchestrator.DefaultDAGPath),
		StatePath: stringOr(vals["--state-json"], devorchestrator.DefaultStatePath),
		// Checked after the four-gate assertion, not before it: the refusal
		// above must stay decidable from disk alone (#135).
		FreshnessCheck: freshnessCheck(repoRoot),
	}
	res, err := devorchestrator.RunGitControl(opts, action)
	if err != nil {
		if refusal, ok := err.(*devorchestrator.GateRefusalError); ok {
			fmt.Fprintf(stderr, "rddev git %s: REFUSED by the four-gate assertion (git was never invoked):\n", action)
			for _, r := range refusal.Reasons {
				fmt.Fprintf(stderr, "  - %s\n", r)
			}
			return exitOperational
		}
		if stale, ok := err.(*staleRefusal); ok {
			fmt.Fprintf(stderr, "rddev git %s: REFUSED — %s\n", action, stale.reason)
			return exitOperational
		}
		return operationalError(stderr, "rddev git "+action, err)
	}
	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		enc.Encode(res)
		return exitOK
	}
	fmt.Fprintf(stdout, "%s: git %s ok — %s (gate status %s)\n", res.TaskID, res.Action, res.Result, res.Gate)
	return exitOK
}
