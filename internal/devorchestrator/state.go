package devorchestrator

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// State is a task status from docs/30_TASK_EXECUTION_STANDARD.md §2.
type State string

// The full state set, in canonical order. Absence of a status entry in
// task_status.json is equivalent to StateTodo.
const (
	StateTodo         State = "todo"
	StateReady        State = "ready"
	StateRunning      State = "running"
	StateWorkerFailed State = "worker_failed"
	StateVerification State = "verification"
	StateRejected     State = "rejected"
	StateBlocked      State = "blocked"
	StateAccepted     State = "accepted"
	StateMerged       State = "merged"
)

// transitions is the legal transition graph, recorded in
// specs/orchestrator/task-state-machine.yaml (L1 decision, T0009; T0012 adds
// rejected -> running for rework/respawn — a rejected task re-dispatching a
// Worker never detours through ready).
//
// accepted -> rejected exists so that an acceptance can be revoked before
// merge. Acceptance is not final until the merge: a required-for-merge gate
// that turns red afterwards — the independent review returning
// request_changes, a CI job failing on the PR, a defect found during the merge
// review — must be able to send the task back. Without it the only escapes
// from accepted are merging something known-bad or hand-editing the state
// file, and T0101 sat in exactly that position: accepted, with a blocking
// review finding, and no legal move.
var transitions = map[State][]State{
	StateTodo:         {StateReady, StateBlocked},
	StateReady:        {StateRunning, StateBlocked},
	StateRunning:      {StateVerification, StateWorkerFailed, StateRejected},
	StateVerification: {StateAccepted, StateRejected},
	StateWorkerFailed: {StateReady},
	StateRejected:     {StateReady, StateRunning},
	StateBlocked:      {StateReady},
	StateAccepted:     {StateMerged, StateRejected},
	StateMerged:       {},
}

// ValidState reports whether s is a known state.
func ValidState(s string) bool {
	switch State(s) {
	case StateTodo, StateReady, StateRunning, StateWorkerFailed,
		StateVerification, StateRejected, StateBlocked, StateAccepted, StateMerged:
		return true
	}
	return false
}

// IllegalTransitionError reports a rejected (never coerced) state transition.
type IllegalTransitionError struct {
	ID   string
	From State
	To   State
}

func (e *IllegalTransitionError) Error() string {
	return fmt.Sprintf("illegal state transition for %s: cannot go from %q to %q (legal transitions from %q: %s)",
		e.ID, e.From, e.To, e.From, legalTransitions(e.From))
}

func legalTransitions(from State) string {
	next := transitions[from]
	parts := make([]string, len(next))
	for i, s := range next {
		parts[i] = string(s)
	}
	sort.Strings(parts) // deterministic error text
	return strings.Join(parts, ", ")
}

// checkTransition returns an error if from -> to is not a legal transition.
func checkTransition(id string, from, to State) error {
	if !ValidState(string(from)) {
		return fmt.Errorf("task %s: unknown current state %q in state file", id, from)
	}
	if !ValidState(string(to)) {
		return fmt.Errorf("task %s: unknown target state %q", id, to)
	}
	for _, legal := range transitions[from] {
		if to == legal {
			return nil
		}
	}
	return &IllegalTransitionError{ID: id, From: from, To: to}
}

// StateChange is one entry of the append-only per-task history.
type StateChange struct {
	From   State  `json:"from"`
	To     State  `json:"to"`
	At     string `json:"at"`               // RFC 3339 UTC
	RunID  string `json:"run_id"`           // carried on every state change
	Reason string `json:"reason,omitempty"` // rejection reason or similar
}

// TaskState is one task's entry in tasks/task_status.json. Fields rddev does
// not model are preserved verbatim through the extra map, so a state change
// never silently drops data the Supervisor or a later rddev version added.
type TaskState struct {
	Status                 State         `json:"status"`
	StartedAt              *string       `json:"started_at,omitempty"`
	CompletedAt            *string       `json:"completed_at,omitempty"`
	Notes                  string        `json:"notes,omitempty"`
	WorkerRunID            string        `json:"worker_run_id,omitempty"`
	AcceptedBySupervisorAt *string       `json:"accepted_by_supervisor_at,omitempty"`
	MergedAt               *string       `json:"merged_at,omitempty"`
	RejectionReason        string        `json:"rejection_reason,omitempty"`
	History                []StateChange `json:"history,omitempty"`

	extra map[string]json.RawMessage
}

// knownTaskStateFields lists the modeled JSON keys; anything else is extra.
var knownTaskStateFields = map[string]bool{
	"status": true, "started_at": true, "completed_at": true, "notes": true,
	"worker_run_id": true, "accepted_by_supervisor_at": true, "merged_at": true,
	"rejection_reason": true, "history": true,
}

// UnmarshalJSON decodes the known fields and stashes unknown ones in extra.
func (ts *TaskState) UnmarshalJSON(data []byte) error {
	type plain TaskState
	var a plain
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*ts = TaskState(a)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for k, v := range raw {
		if !knownTaskStateFields[k] {
			if ts.extra == nil {
				ts.extra = map[string]json.RawMessage{}
			}
			ts.extra[k] = v
		}
	}
	return nil
}

// MarshalJSON emits the known fields plus any preserved unknown ones.
func (ts TaskState) MarshalJSON() ([]byte, error) {
	type plain TaskState
	// marshalNoEscape, not json.Marshal: HTML escaping would rewrite every
	// ">" and "<" in a note (e.g. "todo -> ready") as \u003e, making
	// task_status.json unreadable in its most human-read field.
	b, err := marshalNoEscape(plain(ts))
	if err != nil {
		return nil, err
	}
	if len(ts.extra) == 0 {
		return b, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	for k, v := range ts.extra {
		m[k] = v
	}
	return marshalNoEscape(m)
}

// DependencyError reports a refused `ready` transition: at least one
// dependency is not yet merged (docs/30 §3).
type DependencyError struct {
	ID    string
	Unmet map[string]State
}

func (e *DependencyError) Error() string {
	deps := make([]string, 0, len(e.Unmet))
	for dep := range e.Unmet {
		deps = append(deps, fmt.Sprintf("%s (%s)", dep, e.Unmet[dep]))
	}
	sort.Strings(deps)
	return fmt.Sprintf("task %s cannot become ready: dependencies not merged: %s", e.ID, strings.Join(deps, ", "))
}
