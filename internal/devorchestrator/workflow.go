package devorchestrator

import (
	"strings"
)

// The workflow view (T0012 requirement 7: "Supervisor can resume an
// interrupted workflow from disk alone"). `rddev workflow TASK` rebuilds the
// entire picture of one task — state, gate evidence, rejection reasons, the
// review verdict, the git/PR trail — from the state file and the gate record
// files, and reports the single next action. Nothing lives in memory: a new
// Supervisor session reading the same disk sees the same workflow.

// WorkflowView is the aggregated disk picture of one task.
type WorkflowView struct {
	TaskID      string            `json:"task_id"`
	State       string            `json:"state"`
	Title       string            `json:"title,omitempty"`
	GateStatus  map[string]string `json:"gate_status"`         // G1..G4 from the latest records
	Latest      map[string]string `json:"latest,omitempty"`    // record type -> newest run id
	Rejection   *RejectRecord     `json:"rejection,omitempty"` // newest rejection, if any
	Review      *ReviewRecord     `json:"review,omitempty"`    // newest review verdict, if any
	History     []StateChange     `json:"history,omitempty"`
	NextAction  string            `json:"next_action"`
	NextCommand string            `json:"next_command"`
}

// BuildWorkflow aggregates the on-disk evidence for one task and computes
// the next action. It never writes anything.
func BuildWorkflow(repoRoot, dagPath, statePath, gatesPath, taskID string) (*WorkflowView, error) {
	store, err := OpenStore(dagPath, statePath)
	if err != nil {
		return nil, err
	}
	insp, err := store.Inspect(taskID)
	if err != nil {
		return nil, err
	}
	view := &WorkflowView{
		TaskID:     taskID,
		State:      string(insp.State.Status),
		Title:      insp.Task.Title,
		GateStatus: map[string]string{},
		Latest:     map[string]string{},
		History:    insp.State.History,
	}

	// Latest evidence per record type.
	if coll, ok, err := LatestRecord[CollectRecord](repoRoot, taskID, RecordCollect); err != nil {
		return nil, err
	} else if ok {
		view.Latest[RecordCollect] = coll.RunID
		if coll.Status == "ok" {
			view.GateStatus["G1"] = "passed"
		} else {
			view.GateStatus["G1"] = coll.Status
		}
	} else {
		view.GateStatus["G1"] = "not_run"
	}
	if g2, ok, err := LatestGateRunRecord(repoRoot, taskID, "G2"); err != nil {
		return nil, err
	} else if ok {
		view.Latest["G2"] = g2.RunID
		view.GateStatus["G2"] = g2.Status
	} else {
		view.GateStatus["G2"] = "not_run"
	}
	if g3, ok, err := LatestGateRunRecord(repoRoot, taskID, "G3"); err != nil {
		return nil, err
	} else if ok {
		view.Latest["G3"] = g3.RunID
		view.GateStatus["G3"] = g3.Status
	} else {
		view.GateStatus["G3"] = "not_run"
	}
	if rv, ok, err := LatestRecord[ReviewRecord](repoRoot, taskID, RecordReview); err != nil {
		return nil, err
	} else if ok {
		view.Latest[RecordReview] = rv.RunID
		view.Review = &rv
	}
	if rej, ok, err := LatestRecord[RejectRecord](repoRoot, taskID, RecordReject); err != nil {
		return nil, err
	} else if ok {
		view.Rejection = &rej
	}
	for _, rtype := range []string{RecordAccept, RecordGit} {
		if rec, ok, err := LatestRecord[recordMeta](repoRoot, taskID, rtype); err != nil {
			return nil, err
		} else if ok {
			view.Latest[rtype] = rec.RunID
		}
	}
	if rec, ok, err := LatestRecord[GitRecord](repoRoot, taskID, RecordGit); err != nil {
		return nil, err
	} else if ok {
		view.Latest["git-"+rec.Action] = rec.RunID
	}

	// G4 as seen from disk.
	if g4, err := CheckMergeGate(repoRoot, gatesPath, taskID); err != nil {
		return nil, err
	} else {
		view.GateStatus["G4"] = g4.Status
		if g4.Status != "passed" && len(g4.Reasons) > 0 {
			view.NextCommand = "" // computed below; reasons are the rejection evidence
		}
	}

	// The next action derives from state + evidence, never from memory.
	view.NextAction, view.NextCommand = nextActionFor(repoRoot, taskID, insp.State.Status, view)
	return view, nil
}

// nextActionFor maps (state, evidence) -> next step. The command is the
// exact rddev invocation to resume with.
func nextActionFor(repoRoot, taskID string, state State, view *WorkflowView) (string, string) {
	switch state {
	case StateTodo:
		return "task is todo — mark it ready once dependencies are merged", "rddev task ready " + taskID
	case StateReady:
		return "task is ready — dispatch a Worker", "rddev worker spawn " + taskID
	case StateRunning:
		return "a Worker is running — collect after it exits", "rddev worker list && rddev worker collect " + taskID
	case StateWorkerFailed:
		return "the Worker exited nonzero — the next attempt needs a fresh spawn", "rddev worker spawn " + taskID
	case StateVerification:
		if view.Review != nil && view.Review.Verdict != "approve" {
			return "the review verdict requests changes — reject with the findings and respawn", "rddev task reject " + taskID + " --reason-file <findings>"
		}
		if view.Review == nil {
			if view.GateStatus["G1"] != "passed" {
				return "G1 is not green — the task should not be in verification; reject or re-collect", "rddev worker collect " + taskID
			}
			return "collected clean (G1 green) — spawn an independent Review Worker", "rddev review spawn " + taskID
		}
		// An approve verdict is evidence about ONE code state. A rework after
		// an earlier refusal, a rebase, a baseline advance, a Supervisor glue
		// edit — any change since — leaves it describing code that no longer
		// exists, and the merge gate refuses it by name ("the review verdict is
		// about a DIFFERENT code state"). Advice that sends the Supervisor to
		// `task accept` on a verdict the next gate will reject on sight is not
		// advice: the next action is a fresh review, and the command says so.
		if why, superseded := reviewRecordIsSuperseded(repoRoot, taskID, view.Review); superseded {
			return "the approve verdict is about superseded code (" + why + ") — dispatch a fresh review", "rddev review spawn " + taskID
		}
		return "review approved — run the acceptance gate (G2/G3)", "rddev task accept " + taskID
	case StateRejected:
		reasons := ""
		if view.Rejection != nil && len(view.Rejection.Reasons) > 0 {
			reasons = " — reasons: " + strings.Join(view.Rejection.Reasons, "; ")
		}
		return "rejected" + reasons + " — rework the same Worker (its context survives) or respawn a fresh one", "rddev worker rework " + taskID + "  |  rddev worker respawn " + taskID
	case StateBlocked:
		return "blocked — resolve the blocker, then mark ready", "rddev task ready " + taskID
	case StateAccepted:
		gitActions := view.Latest
		if _, ok := gitActions["git-commit"]; !ok {
			return "accepted and gates green — commit the collected diff", "rddev git commit " + taskID
		}
		if _, ok := gitActions["git-pr-open"]; !ok {
			return "committed — push and open the PR", "rddev pr open " + taskID
		}
		if _, ok := gitActions["git-pr-merge"]; !ok {
			return "PR open — merge (the four-gate assertion runs first)", "rddev pr merge " + taskID
		}
		return "PR merged", ""
	case StateMerged:
		return "done — merged", ""
	}
	return "unknown state " + string(state), "rddev task inspect " + taskID
}

// reviewRecordIsSuperseded reports whether the recorded verdict still describes
// the code in the task's worktree, and why not when it does not.
//
// It asks the merge gate's question (ReviewRecord.DiffSHA against the current
// code identity) from the view's side, so the advice and the refusal cannot
// disagree. Cases it cannot judge answer "not superseded": a worktree that is
// gone, a record with no identity — those produce their own, better reasons at
// the gate, and a view that invents staleness would send the Supervisor to
// re-review work that nothing has invalidated.
func reviewRecordIsSuperseded(repoRoot, taskID string, rv *ReviewRecord) (string, bool) {
	if rv == nil {
		return "", false
	}
	want, err := currentCodeIdentity(repoRoot, taskID)
	if err != nil || want == "" {
		return "", false
	}
	if rv.DiffSHA == "" {
		return "the verdict carries no code identity, so the merge gate refuses it", true
	}
	if rv.DiffSHA == want {
		return "", false
	}
	return "reviewed " + abbrevSHA(rv.DiffSHA) + ", the tree is now " + abbrevSHA(want), true
}
