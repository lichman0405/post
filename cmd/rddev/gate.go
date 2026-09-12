package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lichman0405/post/internal/devorchestrator"
)

const gateUsage = `Usage: rddev gate <command> [TASK] [flags]

Commands:
  list             print the gate spec: G1..G4, required jobs, per-task G3
  run G2|G3 TASK   execute the gate's jobs (CI's exact steps for G2) and
                   record every command, exit code and output log under
                   .rddev/runtime/gates/TASK/; exit 1 when any job is red
  status TASK      print the G1..G4 status derived from the on-disk records
                   (G4 is the merge-gate assertion: it refuses while any
                   required CI job is red or missing)
  records TASK     list the evidence records for one task

Flags:
  --json            machine-readable output
  --gates PATH      gate spec file (default specs/orchestrator/gates.json)
  --run-id ID       run id stamped on the gate-run record (default: generated)

G1 runs at collect and cannot be executed here; G4 asserts against G2 records
and is enforced by rddev git/pr actions, never run by hand.
`

func runGate(args []string, stdout, stderr io.Writer, jsonOut bool) int {
	if wantsHelp(args) {
		fmt.Fprint(stdout, gateUsage)
		return exitOK
	}
	vals, pos, err := parseFlags(args,
		flagSpec{"--json", false},
		flagSpec{"--gates", true},
		flagSpec{"--run-id", true},
	)
	if err != nil {
		return usageError(stderr, err.Error(), gateUsage)
	}
	if _, ok := vals["--json"]; ok {
		jsonOut = true
	}
	if len(pos) == 0 {
		return usageError(stderr, "rddev gate: missing subcommand", gateUsage)
	}
	cmd := pos[0]
	repoRoot, err := os.Getwd()
	if err != nil {
		return operationalError(stderr, "rddev gate", fmt.Errorf("resolving the repo root: %w", err))
	}
	gatesPath := stringOr(vals["--gates"], devorchestrator.DefaultGatesPath)
	spec, err := devorchestrator.LoadGateSpec(devorchestrator.GateSpecPath(repoRoot, gatesPath))
	if err != nil {
		return operationalError(stderr, "rddev gate", err)
	}

	// The TASK (or GATE TASK) positionals: `run` takes both, every other
	// subcommand takes one TASK (or none for `list`).
	rest := pos[1:]
	taskArg := ""
	gate := ""
	switch cmd {
	case "run":
		if len(rest) != 2 {
			return usageError(stderr, "rddev gate run: usage: rddev gate run G2|G3 TASK", gateUsage)
		}
		gate, taskArg = rest[0], rest[1]
	case "list":
		if len(rest) != 0 {
			return usageError(stderr, "rddev gate list: takes no TASK argument", gateUsage)
		}
	case "status", "records":
		if len(rest) != 1 || rest[0] == "" {
			return usageError(stderr, fmt.Sprintf("rddev gate %s: missing TASK", cmd), gateUsage)
		}
		taskArg = rest[0]
	default:
		return usageError(stderr, fmt.Sprintf("rddev gate: unknown subcommand %q", cmd), gateUsage)
	}

	switch cmd {
	case "list":
		return gateList(spec, stdout, stderr, jsonOut)
	case "run":
		if gate != "G2" && gate != "G3" {
			return usageError(stderr, fmt.Sprintf("rddev gate run: gate %q is not executable (G1 runs at collect; G4 asserts inside rddev git/pr)", gate), gateUsage)
		}
		res, err := devorchestrator.RunGate(&devorchestrator.GateRunOpts{
			RepoRoot: repoRoot, GatesPath: gatesPath, TaskID: taskArg, Gate: gate, RunID: vals["--run-id"],
		})
		if err != nil {
			return operationalError(stderr, "rddev gate run", err)
		}
		if jsonOut {
			enc := json.NewEncoder(stdout)
			enc.SetEscapeHTML(false)
			enc.Encode(res)
		} else {
			for _, j := range res.Jobs {
				fmt.Fprintf(stdout, "%s\t%s\t%s\n", j.Job, j.Status, res.RecordPath)
				for _, s := range j.Steps {
					fmt.Fprintf(stdout, "  step %02d exit=%d skipped=%v\n    $ %s\n    log: %s\n",
						s.Index, s.Exit, s.Skipped, s.Run, s.OutputFile)
				}
			}
			fmt.Fprintf(stdout, "%s %s (run_id=%s, record=%s)\n", res.Gate, res.Status, res.RunID, res.RecordPath)
		}
		if res.Status == "failed" {
			return exitOperational
		}
		return exitOK
	case "status":
		if taskArg == "" {
			return usageError(stderr, "rddev gate status: missing TASK", gateUsage)
		}
		status, err := devorchestrator.AcceptGateStatus(repoRoot, gatesPath, taskArg)
		if err != nil {
			return operationalError(stderr, "rddev gate status", err)
		}
		g4, err := devorchestrator.CheckMergeGate(repoRoot, gatesPath, taskArg)
		if err != nil {
			return operationalError(stderr, "rddev gate status", err)
		}
		if jsonOut {
			type gateStatusOut struct {
				TaskID string            `json:"task_id"`
				Gates  map[string]string `json:"gates"`
				G4     struct {
					Status  string   `json:"status"`
					Checks  []string `json:"checks"`
					Reasons []string `json:"reasons,omitempty"`
				} `json:"g4"`
			}
			out := gateStatusOut{TaskID: taskArg, Gates: status}
			out.G4.Status = g4.Status
			out.G4.Checks = g4.Checks
			out.G4.Reasons = g4.Reasons
			enc := json.NewEncoder(stdout)
			enc.SetEscapeHTML(false)
			enc.Encode(out)
			return exitOK
		}
		for _, g := range []string{"G1", "G2", "G3", "G4"} {
			fmt.Fprintf(stdout, "%s\t%s\n", g, status[g])
		}
		if g4.Status != "passed" {
			fmt.Fprintf(stdout, "merge gate: REFUSED\n")
			for _, r := range g4.Reasons {
				fmt.Fprintf(stdout, "  - %s\n", r)
			}
		} else {
			fmt.Fprintf(stdout, "merge gate: passed (all required CI jobs green in the latest G2 record)\n")
		}
		return exitOK
	case "records":
		if taskArg == "" {
			return usageError(stderr, "rddev gate records: missing TASK", gateUsage)
		}
		names, err := devorchestrator.SortedRecordNames(repoRoot, taskArg, "")
		if err != nil {
			return operationalError(stderr, "rddev gate records", err)
		}
		if len(names) == 0 {
			fmt.Fprintf(stdout, "no gate records for %s\n", taskArg)
			return exitOK
		}
		if jsonOut {
			enc := json.NewEncoder(stdout)
			enc.SetEscapeHTML(false)
			enc.Encode(struct {
				TaskID  string   `json:"task_id"`
				Records []string `json:"records"`
			}{TaskID: taskArg, Records: names})
			return exitOK
		}
		for _, n := range names {
			fmt.Fprintln(stdout, n)
		}
		return exitOK
	default:
		return usageError(stderr, fmt.Sprintf("rddev gate: unknown subcommand %q", cmd), gateUsage)
	}
}

// gateList prints the gate spec: the four gates, the required jobs and any
// per-task G3 override.
func gateList(spec *devorchestrator.GateSpec, stdout, stderr io.Writer, jsonOut bool) int {
	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		enc.Encode(spec)
		return exitOK
	}
	fmt.Fprintf(stdout, "required jobs (the merge gate demands every one green):\n  %s\n", strings.Join(spec.RequiredJobs, ", "))
	for _, g := range []string{"G1", "G2", "G3", "G4"} {
		def := spec.Gates[g]
		fmt.Fprintf(stdout, "%s\t%s\n", g, def.Description)
		if len(def.RunsJobs) > 0 {
			fmt.Fprintf(stdout, "    runs: %s\n", strings.Join(def.RunsJobs, ", "))
		}
		if len(def.AssertsJobs) > 0 {
			fmt.Fprintf(stdout, "    asserts: %s\n", strings.Join(def.AssertsJobs, ", "))
		}
	}
	fmt.Fprintf(stdout, "review required for merge: %v\n", spec.Review.RequiredForMerge)
	if len(spec.TaskOverrides) > 0 {
		for id, ov := range spec.TaskOverrides {
			parts := []string{}
			if len(ov.G3Jobs) > 0 {
				parts = append(parts, "g3_jobs="+strings.Join(ov.G3Jobs, ","))
			}
			if ov.ReviewRequiredMerge != nil {
				parts = append(parts, fmt.Sprintf("review_required=%v", *ov.ReviewRequiredMerge))
			}
			fmt.Fprintf(stdout, "override %s: %s\n", id, strings.Join(parts, " "))
		}
	}
	return exitOK
}
