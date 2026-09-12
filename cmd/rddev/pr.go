package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/lichman0405/post/internal/devorchestrator"
)

const prUsage = `Usage: rddev pr <command> TASK [flags]

Commands:
  open TASK     open a pull request for the task branch (base main)
  merge TASK    squash-merge the task's PR and record accepted -> merged
  status TASK   print the four-gate assertion that open/merge will run

PR management is Supervisor-only (docs/61 §3). open and merge run the
four-gate assertion first and REFUSE — before gh is ever invoked — while any
gate is red or missing. A merge is the point of no return: it demands every
required CI job green in the task's latest G2 record, a green collect that is
not newer than that record, a green G3 where the task requires one, and an
approving review verdict where review is required for merge.

Flags:
  --json            machine-readable output
  --tasks-json PATH task DAG file (default tasks/tasks.json)
  --state-json PATH task status file (default tasks/task_status.json)
  --gates PATH      gate spec file (default specs/orchestrator/gates.json)
  --run-id ID       run id stamped on the PR record (default: generated)
  --title TEXT      PR title (default: rendered from the task DAG)
  --body TEXT       PR body (default: rendered from the task DAG)
`

// runPR replaces the T0012 stub: real PR open/merge with the gate assertion.
func runPR(args []string, stdout, stderr io.Writer, jsonOut bool) int {
	if wantsHelp(args) {
		fmt.Fprint(stdout, prUsage)
		return exitOK
	}
	vals, pos, err := parseFlags(args,
		flagSpec{"--json", false},
		flagSpec{"--tasks-json", true},
		flagSpec{"--state-json", true},
		flagSpec{"--gates", true},
		flagSpec{"--run-id", true},
		flagSpec{"--title", true},
		flagSpec{"--body", true},
	)
	if err != nil {
		return usageError(stderr, err.Error(), prUsage)
	}
	if _, ok := vals["--json"]; ok {
		jsonOut = true
	}
	if len(pos) == 0 {
		return usageError(stderr, "rddev pr: missing subcommand", prUsage)
	}
	cmd, taskArg := pos[0], ""
	if len(pos) > 1 {
		taskArg = pos[1]
		if len(pos) > 2 {
			return usageError(stderr, fmt.Sprintf("rddev pr %s: unexpected argument %q", cmd, pos[2]), prUsage)
		}
	}
	if taskArg == "" {
		return usageError(stderr, fmt.Sprintf("rddev pr %s: missing TASK", cmd), prUsage)
	}
	repoRoot, err := os.Getwd()
	if err != nil {
		return operationalError(stderr, "rddev pr", fmt.Errorf("resolving the repo root: %w", err))
	}
	gatesPath := vals["--gates"]
	if cmd == "status" {
		return prStatus(repoRoot, gatesPath, taskArg, stdout, stderr, jsonOut)
	}
	action := map[string]string{"open": "pr-open", "merge": "pr-merge"}[cmd]
	if action == "" {
		return usageError(stderr, fmt.Sprintf("rddev pr: unknown subcommand %q (valid: open, merge, status)", cmd), prUsage)
	}
	opts := &devorchestrator.GitControlOpts{
		RepoRoot:  repoRoot,
		GatesPath: gatesPath,
		TaskID:    taskArg,
		RunID:     vals["--run-id"],
		PRTitle:   vals["--title"],
		PRBody:    vals["--body"],
		DagPath:   stringOr(vals["--tasks-json"], devorchestrator.DefaultDAGPath),
		StatePath: stringOr(vals["--state-json"], devorchestrator.DefaultStatePath),
	}
	res, err := devorchestrator.RunGitControl(opts, action)
	if err != nil {
		if refusal, ok := err.(*devorchestrator.GateRefusalError); ok {
			fmt.Fprintf(stderr, "rddev pr %s: REFUSED by the four-gate assertion (gh was never invoked):\n", cmd)
			for _, r := range refusal.Reasons {
				fmt.Fprintf(stderr, "  - %s\n", r)
			}
			return exitOperational
		}
		return operationalError(stderr, "rddev pr "+cmd, err)
	}
	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		enc.Encode(res)
		return exitOK
	}
	fmt.Fprintf(stdout, "%s: pr %s ok — %s (gate status %s)\n", res.TaskID, res.Action, res.Result, res.Gate)
	return exitOK
}

// prStatus prints the four-gate assertion open/merge would run — the refusal
// reasons are the same the actions would print.
func prStatus(repoRoot, gatesPath, taskID string, stdout, stderr io.Writer, jsonOut bool) int {
	g4, err := devorchestrator.CheckMergeGate(repoRoot, gatesPath, taskID)
	if err != nil {
		return operationalError(stderr, "rddev pr status", err)
	}
	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		enc.Encode(g4)
		return exitOK
	}
	for _, c := range g4.Checks {
		fmt.Fprintf(stdout, "check: %s\n", c)
	}
	if g4.Status == "passed" {
		fmt.Fprintf(stdout, "%s: merge gate passed — pr open/merge will proceed\n", taskID)
		return exitOK
	}
	fmt.Fprintf(stdout, "%s: merge gate REFUSES pr open/merge:\n", taskID)
	for _, r := range g4.Reasons {
		fmt.Fprintf(stdout, "  - %s\n", r)
	}
	return exitOperational
}
