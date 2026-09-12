package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/lichman0405/post/internal/devorchestrator"
)

// runWorkerCollect implements `rddev worker collect TASK` (T0011): the
// collection gate that re-verifies the Worker boundary from real state —
// HEAD/refs/scope/RESULT schema/secrets/residue — instead of trusting the
// Worker's own report. Exit code: 0 when the collection is clean (the task
// moves to verification), 1 when the run is rejected or failed (the report
// carries the reasons), 2 on usage errors. The report is also written to the
// task dir as collect-report.json for later inspection.
func runWorkerCollect(vals map[string]string, taskID, repoRoot string, stdout, stderr io.Writer, jsonOut bool) int {
	opts := &devorchestrator.CollectOpts{
		RepoRoot:  repoRoot,
		DagPath:   stringOr(vals["--tasks-json"], devorchestrator.DefaultDAGPath),
		StatePath: stringOr(vals["--state-json"], devorchestrator.DefaultStatePath),
		TaskID:    taskID,
		RunID:     vals["--run-id"],
	}
	report, err := devorchestrator.Collect(opts)

	if jsonOut {
		if report != nil {
			enc := json.NewEncoder(stdout)
			enc.SetEscapeHTML(false)
			enc.Encode(report)
		}
		if err != nil {
			return operationalError(stderr, "rddev worker collect", err)
		}
		if report.Status == "ok" {
			return exitOK
		}
		return exitOperational
	}

	if report != nil {
		fmt.Fprintf(stdout, "%s: collect %s (state -> %s)\n", report.TaskID, report.Status, report.StateTransition)
		for _, c := range report.Checks {
			mark := "ok"
			if c.Status == "failed" {
				mark = "FAIL"
			}
			fmt.Fprintf(stdout, "  [%s] %s: %s\n", mark, c.Name, c.Detail)
		}
		fmt.Fprintf(stdout, "  changed: %d file(s)\n", len(report.FilesChanged))
		for _, p := range report.FilesChanged {
			fmt.Fprintf(stdout, "    %s\n", p)
		}
		for _, p := range report.Residue {
			fmt.Fprintf(stdout, "  residue: pid %d (%s)\n", p.PID, p.Cmdline)
		}
		for _, w := range report.ListenerWarnings {
			fmt.Fprintf(stdout, "  listener appeared during the run: %s owned by pid %d (%s) — %s\n", w.Addr, w.PID, w.Cmdline, w.Attribution)
		}
	}
	if err != nil {
		return operationalError(stderr, "rddev worker collect", err)
	}
	if report.Status == "ok" {
		return exitOK
	}
	return exitOperational
}
