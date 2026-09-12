package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/lichman0405/post/internal/devorchestrator"
)

const taskUsage = `Usage: rddev task <command> [TASK] [flags]

Commands:
  next              list dispatchable tasks (todo/ready with all dependencies merged)
  ready [TASK]      without TASK: list the ready pool; with TASK: mark it ready
                    (requires all dependencies merged)
  inspect TASK      print the DAG entry and the full recorded state
  verify TASK       running -> verification
  accept TASK       run the acceptance gate (G2 = CI's exact six jobs, then G3
                    where the task defines one) and verification -> accepted;
                    REFUSES (exit 1, state unchanged, AcceptRecord evidence
                    written) while any gate is red or missing
  reject TASK       running|verification -> rejected (requires --reason or
                    --reason-file); the rejection is recorded as a RejectRecord
                    (reasons + evidence paths) that rework/respawn carry
  merged TASK       accepted -> merged (Supervisor merge bookkeeping)

Flags:
  --json                machine-readable output
  --tasks-json PATH     task DAG file (default tasks/tasks.json)
  --state-json PATH     task status file (default tasks/task_status.json)
  --gates PATH          gate spec file (default specs/orchestrator/gates.json)
  --run-id ID           run id stamped on state changes (default: generated)
  --reason TEXT         rejection reason
  --reason-file FILE    rejection reason read from FILE

Illegal transitions are rejected (exit 1) and the state file is left unchanged.
State writes are atomic and serialized on an exclusive lock; run from the repo
root or pass --tasks-json/--state-json.
`

// taskRunner carries the resolved store and streams for one rddev task call.
type taskRunner struct {
	store     *devorchestrator.Store
	stdout    io.Writer
	stderr    io.Writer
	jsonOut   bool
	runID     string
	gatesPath string
	repoRoot  string
}

func runTask(args []string, stdout, stderr io.Writer, jsonOut bool) int {
	if wantsHelp(args) {
		fmt.Fprint(stdout, taskUsage)
		return exitOK
	}
	vals, pos, err := parseFlags(args,
		flagSpec{"--json", false},
		flagSpec{"--tasks-json", true},
		flagSpec{"--state-json", true},
		flagSpec{"--gates", true},
		flagSpec{"--run-id", true},
		flagSpec{"--reason", true},
		flagSpec{"--reason-file", true},
	)
	if err != nil {
		return usageError(stderr, err.Error(), taskUsage)
	}
	if _, ok := vals["--json"]; ok {
		jsonOut = true
	}
	if len(pos) == 0 {
		return usageError(stderr, "rddev task: missing subcommand", taskUsage)
	}
	cmd, taskArg := pos[0], ""
	if len(pos) > 1 {
		taskArg = pos[1]
		if len(pos) > 2 {
			return usageError(stderr, fmt.Sprintf("rddev task %s: unexpected argument %q", cmd, pos[2]), taskUsage)
		}
	}

	dagPath := stringOr(vals["--tasks-json"], devorchestrator.DefaultDAGPath)
	statePath := stringOr(vals["--state-json"], devorchestrator.DefaultStatePath)
	store, err := devorchestrator.OpenStore(dagPath, statePath)
	if err != nil {
		return operationalError(stderr, "rddev task", err)
	}
	repoRoot, err := os.Getwd()
	if err != nil {
		return operationalError(stderr, "rddev task", fmt.Errorf("resolving the repo root: %w", err))
	}
	tr := &taskRunner{
		store:     store,
		stdout:    stdout,
		stderr:    stderr,
		jsonOut:   jsonOut,
		runID:     stringOr(vals["--run-id"], devorchestrator.NewRunID()),
		gatesPath: vals["--gates"],
		repoRoot:  repoRoot,
	}

	switch cmd {
	case "next":
		return tr.next()
	case "ready":
		if taskArg == "" {
			return tr.readyList()
		}
		return tr.transition(taskArg, devorchestrator.StateReady, "")
	case "inspect":
		if taskArg == "" {
			return usageError(stderr, "rddev task inspect: missing TASK", taskUsage)
		}
		return tr.inspect(taskArg)
	case "verify":
		return tr.transition(taskArg, devorchestrator.StateVerification, "")
	case "accept":
		return tr.accept(taskArg)
	case "reject":
		if taskArg == "" {
			return usageError(stderr, "rddev task reject: missing TASK", taskUsage)
		}
		reason := vals["--reason"]
		if reason == "" {
			if rf := vals["--reason-file"]; rf != "" {
				data, err := os.ReadFile(rf)
				if err != nil {
					return operationalError(stderr, "rddev task reject", fmt.Errorf("reading reason file %s: %w", rf, err))
				}
				reason = string(data)
			}
		}
		if reason == "" {
			return usageError(stderr, "rddev task reject: requires --reason TEXT or --reason-file FILE", taskUsage)
		}
		return tr.reject(taskArg, reason)
	case "merged":
		return tr.transition(taskArg, devorchestrator.StateMerged, "")
	default:
		return usageError(stderr, fmt.Sprintf("rddev task: unknown subcommand %q", cmd), taskUsage)
	}
}

// next prints the dispatch pool (tasks whose dependencies are all satisfied).
func (tr *taskRunner) next() int {
	next, err := tr.store.Next()
	if err != nil {
		return operationalError(tr.stderr, "rddev task next", err)
	}
	if tr.jsonOut {
		tr.writeJSON(struct {
			Next []devorchestrator.NextTask `json:"next"`
		}{Next: next})
		return exitOK
	}
	for _, n := range next {
		fmt.Fprintf(tr.stdout, "%s\t%s\t%s\t%s\n", n.ID, n.Status, n.Phase, n.Title)
	}
	return exitOK
}

// readyList prints the current ready pool.
func (tr *taskRunner) readyList() int {
	ready, err := tr.store.ReadyList()
	if err != nil {
		return operationalError(tr.stderr, "rddev task ready", err)
	}
	if tr.jsonOut {
		tr.writeJSON(struct {
			Ready []devorchestrator.NextTask `json:"ready"`
		}{Ready: ready})
		return exitOK
	}
	for _, n := range ready {
		fmt.Fprintf(tr.stdout, "%s\t%s\t%s\t%s\n", n.ID, n.Status, n.Phase, n.Title)
	}
	return exitOK
}

// inspect prints the DAG entry plus the full recorded state.
func (tr *taskRunner) inspect(id string) int {
	res, err := tr.store.Inspect(id)
	if err != nil {
		return operationalError(tr.stderr, "rddev task inspect", err)
	}
	if tr.jsonOut {
		tr.writeJSON(res)
		return exitOK
	}
	t, s := res.Task, res.State
	fmt.Fprintf(tr.stdout, "%s\t%s\t%s\t%s\n", t.ID, s.Status, t.Phase, t.Title)
	if len(t.Dependencies) > 0 {
		fmt.Fprintf(tr.stdout, "  dependencies: %v\n", t.Dependencies)
	}
	for _, h := range s.History {
		reason := ""
		if h.Reason != "" {
			reason = " reason=" + h.Reason
		}
		fmt.Fprintf(tr.stdout, "  %s: %s -> %s run_id=%s%s\n", h.At, h.From, h.To, h.RunID, reason)
	}
	return exitOK
}

// transition applies a guarded state change and reports it.
func (tr *taskRunner) transition(id string, to devorchestrator.State, reason string) int {
	if id == "" {
		return usageError(tr.stderr, fmt.Sprintf("rddev task %s: missing TASK", to), taskUsage)
	}
	res, err := tr.store.Transition(id, to, tr.runID, reason)
	if err != nil {
		return operationalError(tr.stderr, "rddev task "+string(to), err)
	}
	if tr.jsonOut {
		tr.writeJSON(res)
		return exitOK
	}
	fmt.Fprintf(tr.stdout, "%s: %s -> %s (run_id=%s, at=%s)\n", res.TaskID, res.From, res.To, res.RunID, res.At)
	return exitOK
}

// accept runs the acceptance gate (T0012): G2 = CI's exact six jobs (a fresh
// all-green G2 record at/after the latest collect is reused; anything else is
// executed now), then G3 where the task defines one. Any red or missing gate
// refuses the verification -> accepted transition with the refusal recorded as
// an AcceptRecord — accepting on a subset-G2 is the defect this makes
// impossible.
func (tr *taskRunner) accept(id string) int {
	if id == "" {
		return usageError(tr.stderr, "rddev task accept: missing TASK", taskUsage)
	}
	repoRoot := tr.repoRoot
	if repoRoot == "" {
		var err error
		repoRoot, err = os.Getwd()
		if err != nil {
			return operationalError(tr.stderr, "rddev task accept", fmt.Errorf("resolving the repo root: %w", err))
		}
	}
	gatesPath := tr.gatesPath
	runID := tr.runID
	// A wrong-state accept is refused by the transition machinery BEFORE any
	// gate work: an illegal transition must report the illegal transition,
	// not a missing gate spec (and must never burn a six-job G2 run first).
	insp, err := tr.store.Inspect(id)
	if err != nil {
		return operationalError(tr.stderr, "rddev task accept", err)
	}
	if insp.State.Status != devorchestrator.StateVerification {
		_, err := tr.store.Transition(id, devorchestrator.StateAccepted, runID, "")
		return operationalError(tr.stderr, "rddev task accept", err)
	}
	recordRefusal := func(status map[string]string, reasons []string) {
		if _, err := devorchestrator.WriteRecord(repoRoot, id, devorchestrator.RecordAccept, runID, devorchestrator.NewAcceptRecord(id, runID, "refused", status, reasons)); err != nil {
			fmt.Fprintf(tr.stderr, "rddev task accept: writing the refusal evidence: %v\n", err)
		}
	}

	// G1: the task must have collected clean (it is in verification, but the
	// evidence must agree — state and records are both on disk).
	status, err := devorchestrator.AcceptGateStatus(repoRoot, gatesPath, id)
	if err != nil {
		return operationalError(tr.stderr, "rddev task accept", err)
	}
	reasons := []string{}
	if status["G1"] != "passed" {
		reasons = append(reasons, "G1 is not green: no ok collect record exists — collect the Worker first (rddev worker collect "+id+")")
	}

	// G2: CI's exact six jobs, all green.
	g2, err := devorchestrator.EnsureG2Green(&devorchestrator.GateRunOpts{
		RepoRoot: repoRoot, GatesPath: gatesPath, TaskID: id, RunID: runID,
	})
	if err != nil {
		status["G2"] = "failed"
		reasons = append(reasons, "G2 could not run: "+err.Error())
		recordRefusal(status, reasons)
		return operationalError(tr.stderr, "rddev task accept", err)
	}
	if g2.Status != "passed" {
		status["G2"] = "failed"
		reasons = append(reasons, "G2 (CI's exact six jobs) is red in run "+g2.RunID+" — see "+g2.RecordPath+" and the step logs")
		recordRefusal(status, reasons)
		fmt.Fprintf(tr.stderr, "rddev task accept: REFUSED — G2 is red (state unchanged):\n")
		for _, r := range reasons {
			fmt.Fprintf(tr.stderr, "  - %s\n", r)
		}
		return exitOperational
	}
	status["G2"] = "passed"

	// G3: run when the task defines integration/E2E jobs and no green run
	// covers them at/after the latest collect.
	spec, err := devorchestrator.LoadGateSpec(devorchestrator.GateSpecPath(repoRoot, stringOr(gatesPath, devorchestrator.DefaultGatesPath)))
	if err != nil {
		return operationalError(tr.stderr, "rddev task accept", err)
	}
	g3Jobs, err := spec.JobsForGate("G3", id)
	if err != nil {
		return operationalError(tr.stderr, "rddev task accept", err)
	}
	if len(g3Jobs) > 0 {
		needRun := true
		if g3, ok, err := devorchestrator.LatestGateRunRecord(repoRoot, id, "G3"); err != nil {
			return operationalError(tr.stderr, "rddev task accept", err)
		} else if ok && g3.Status == "passed" {
			covered := true
			for _, j := range g3Jobs {
				found := false
				for _, jr := range g3.Jobs {
					if jr.Job == j && jr.Status == "passed" {
						found = true
					}
				}
				if !found {
					covered = false
				}
			}
			if covered {
				if coll, cok, err := devorchestrator.LatestRecord[devorchestrator.CollectRecord](repoRoot, id, devorchestrator.RecordCollect); err != nil {
					return operationalError(tr.stderr, "rddev task accept", err)
				} else if cok && g3.At >= coll.At {
					needRun = false
				}
			}
		}
		if needRun {
			g3, err := devorchestrator.RunGate(&devorchestrator.GateRunOpts{
				RepoRoot: repoRoot, GatesPath: gatesPath, TaskID: id, Gate: "G3", RunID: runID,
			})
			if err != nil {
				status["G3"] = "failed"
				reasons = append(reasons, "G3 could not run: "+err.Error())
				recordRefusal(status, reasons)
				return operationalError(tr.stderr, "rddev task accept", err)
			}
			if g3.Status != "passed" {
				status["G3"] = "failed"
				reasons = append(reasons, "G3 (task integration/E2E) is red in run "+g3.RunID+" — see "+g3.RecordPath)
				recordRefusal(status, reasons)
				fmt.Fprintf(tr.stderr, "rddev task accept: REFUSED — G3 is red (state unchanged):\n")
				for _, r := range reasons {
					fmt.Fprintf(tr.stderr, "  - %s\n", r)
				}
				return exitOperational
			}
		}
		status["G3"] = "passed"
	} else {
		status["G3"] = "not_required"
	}

	if len(reasons) > 0 {
		recordRefusal(status, reasons)
		fmt.Fprintf(tr.stderr, "rddev task accept: REFUSED (state unchanged):\n")
		for _, r := range reasons {
			fmt.Fprintf(tr.stderr, "  - %s\n", r)
		}
		return exitOperational
	}
	// G4: run the SAME assertion `rddev pr open` and `rddev pr merge` run, and
	// require it BEFORE acceptance rather than after.
	//
	// Accepting without it deadlocked T0101. accept hard-coded G4 as
	// "not_required" and moved the task to accepted; only then did `pr open`
	// report that a review verdict is required for merge. A Review Worker may
	// review a task in verification only, so the requirement became
	// unsatisfiable in an accepted state — accepted and unmergeable at once,
	// with no legal transition out. Acceptance must not be reachable in an
	// order that makes a later gate impossible to satisfy.
	mergeRes, err := devorchestrator.CheckMergeGate(repoRoot, gatesPath, id)
	if err != nil {
		return operationalError(tr.stderr, "rddev task accept", err)
	}
	if mergeRes.Status != "passed" {
		status["G4"] = "failed"
		reasons = append(reasons, mergeRes.Reasons...)
		recordRefusal(status, reasons)
		fmt.Fprintf(tr.stderr, "rddev task accept: REFUSED — the merge gate (G4) is not satisfied (state unchanged):\n")
		for _, r := range reasons {
			fmt.Fprintf(tr.stderr, "  - %s\n", r)
		}
		return exitOperational
	}
	status["G4"] = "passed"

	res, err := tr.store.Transition(id, devorchestrator.StateAccepted, runID, "")
	if err != nil {
		return operationalError(tr.stderr, "rddev task accept", err)
	}
	// The accept evidence is written only after the transition succeeded.
	if _, err := devorchestrator.WriteRecord(repoRoot, id, devorchestrator.RecordAccept, runID, devorchestrator.NewAcceptRecord(id, runID, "accepted", status, nil)); err != nil {
		return operationalError(tr.stderr, "rddev task accept", err)
	}
	if tr.jsonOut {
		tr.writeJSON(struct {
			*devorchestrator.TransitionResult
			Gates map[string]string `json:"gates"`
		}{TransitionResult: res, Gates: status})
		return exitOK
	}
	fmt.Fprintf(tr.stdout, "%s: %s -> %s (run_id=%s, gates: G1=%s G2=%s G3=%s G4=%s)\n",
		res.TaskID, res.From, res.To, res.RunID, status["G1"], status["G2"], status["G3"], status["G4"])
	return exitOK
}

// reject applies running|verification -> rejected and records the RejectRecord
// evidence (reasons + the evidence paths that back them) — the record rework
// and respawn carry into the Worker's next prompt.
func (tr *taskRunner) reject(id, reason string) int {
	res, err := tr.store.Transition(id, devorchestrator.StateRejected, tr.runID, reason)
	if err != nil {
		return operationalError(tr.stderr, "rddev task reject", err)
	}
	repoRoot := tr.repoRoot
	if repoRoot == "" {
		var err error
		repoRoot, err = os.Getwd()
		if err != nil {
			return operationalError(tr.stderr, "rddev task reject", fmt.Errorf("resolving the repo root: %w", err))
		}
	}
	// The evidence paths backing the rejection: the collect report (G1), the
	// review verdict and the latest G2 record, whatever exists on disk.
	evidence := []string{}
	if rec, err := devorchestrator.LoadRegistry(repoRoot, id); err == nil && rec != nil {
		report := filepath.Join(rec.ResultDir, "collect-report.json")
		if _, err := os.Stat(report); err == nil {
			evidence = append(evidence, report)
		}
	}
	if rv, ok, err := devorchestrator.LatestRecord[devorchestrator.ReviewRecord](repoRoot, id, devorchestrator.RecordReview); err == nil && ok && rv.VerdictPath != "" {
		evidence = append(evidence, rv.VerdictPath)
	}
	if g2, ok, err := devorchestrator.LatestGateRunRecord(repoRoot, id, "G2"); err == nil && ok {
		evidence = append(evidence, filepath.Join(devorchestrator.GatesDir(repoRoot, id), devorchestrator.RecordGateRun+"-"+g2.RunID+".json"))
	}
	reasons := []string{reason}
	if _, err := devorchestrator.WriteRecord(repoRoot, id, devorchestrator.RecordReject, tr.runID, devorchestrator.NewRejectRecord(id, tr.runID, reasons, evidence)); err != nil {
		return operationalError(tr.stderr, "rddev task reject", err)
	}
	if tr.jsonOut {
		tr.writeJSON(struct {
			*devorchestrator.TransitionResult
			Evidence []string `json:"evidence"`
		}{TransitionResult: res, Evidence: evidence})
		return exitOK
	}
	fmt.Fprintf(tr.stdout, "%s: %s -> %s (run_id=%s, at=%s, reason recorded)\n", res.TaskID, res.From, res.To, res.RunID, res.At)
	for _, e := range evidence {
		fmt.Fprintf(tr.stdout, "  evidence: %s\n", e)
	}
	return exitOK
}

// writeJSON emits v as compact JSON with a trailing newline (raw Unicode).
func (tr *taskRunner) writeJSON(v any) {
	enc := json.NewEncoder(tr.stdout)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

// usageError prints a usage error and the command usage to stderr (exit 2).
func usageError(stderr io.Writer, msg, usageText string) int {
	fmt.Fprintf(stderr, "rddev: %s\n\n%s", msg, usageText)
	return exitUsage
}

// operationalError prints a runtime failure to stderr (exit 1).
func operationalError(stderr io.Writer, where string, err error) int {
	fmt.Fprintf(stderr, "%s: %v\n", where, err)
	return exitOperational
}

func stringOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
