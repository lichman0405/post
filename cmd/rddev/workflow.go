package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/lichman0405/post/internal/devorchestrator"
)

const workflowUsage = `Usage: rddev workflow TASK [flags]

Rebuild the whole picture of one task from disk alone: state, the four-gate
status, the latest evidence records, the rejection reasons, the review verdict
and the git/PR trail — plus the single next action and its exact rddev
command. An interrupted Supervisor session resumes by reading this, never by
remembering. Nothing is written.

Flags:
  --json            machine-readable output
  --tasks-json PATH task DAG file (default tasks/tasks.json)
  --state-json PATH task status file (default tasks/task_status.json)
  --gates PATH      gate spec file (default specs/orchestrator/gates.json)
`

func runWorkflow(args []string, stdout, stderr io.Writer, jsonOut bool) int {
	if wantsHelp(args) {
		fmt.Fprint(stdout, workflowUsage)
		return exitOK
	}
	vals, pos, err := parseFlags(args,
		flagSpec{"--json", false},
		flagSpec{"--tasks-json", true},
		flagSpec{"--state-json", true},
		flagSpec{"--gates", true},
	)
	if err != nil {
		return usageError(stderr, err.Error(), workflowUsage)
	}
	if _, ok := vals["--json"]; ok {
		jsonOut = true
	}
	if len(pos) == 0 {
		return usageError(stderr, "rddev workflow: missing TASK", workflowUsage)
	}
	if len(pos) > 1 {
		return usageError(stderr, fmt.Sprintf("rddev workflow: unexpected argument %q", pos[1]), workflowUsage)
	}
	repoRoot, err := os.Getwd()
	if err != nil {
		return operationalError(stderr, "rddev workflow", fmt.Errorf("resolving the repo root: %w", err))
	}
	view, err := devorchestrator.BuildWorkflow(repoRoot,
		stringOr(vals["--tasks-json"], devorchestrator.DefaultDAGPath),
		stringOr(vals["--state-json"], devorchestrator.DefaultStatePath),
		vals["--gates"], pos[0])
	if err != nil {
		return operationalError(stderr, "rddev workflow", err)
	}
	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		enc.Encode(view)
		return exitOK
	}
	fmt.Fprintf(stdout, "%s\t%s\t%s\n", view.TaskID, view.State, view.Title)
	for _, g := range []string{"G1", "G2", "G3", "G4"} {
		fmt.Fprintf(stdout, "  %s: %s", g, view.GateStatus[g])
		if runID, ok := view.Latest["G2"]; ok && g == "G2" {
			fmt.Fprintf(stdout, " (run %s)", runID)
		}
		fmt.Fprintln(stdout)
	}
	if view.Rejection != nil {
		fmt.Fprintf(stdout, "  rejected (run %s): %s\n", view.Rejection.RunID, joinReasons(view.Rejection.Reasons))
		for _, e := range view.Rejection.Evidence {
			fmt.Fprintf(stdout, "    evidence: %s\n", e)
		}
	}
	if view.Review != nil {
		fmt.Fprintf(stdout, "  review: %s (%d blocking, %d major) — %s\n",
			view.Review.Verdict, view.Review.BlockingFindings, view.Review.MajorFindings, view.Review.Summary)
	}
	for _, h := range view.History {
		fmt.Fprintf(stdout, "  %s: %s -> %s\n", h.At, h.From, h.To)
	}
	fmt.Fprintf(stdout, "next: %s\n", view.NextAction)
	fmt.Fprintf(stdout, "  $ %s\n", view.NextCommand)
	return exitOK
}

func joinReasons(rs []string) string {
	out := ""
	for i, r := range rs {
		if i > 0 {
			out += "; "
		}
		out += r
	}
	return out
}
