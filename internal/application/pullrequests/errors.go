package pullrequests

import (
	"errors"
	"fmt"

	"github.com/lichman0405/post/internal/domain"
)

// Sentinel errors the service maps for the transport layer (docs/45: the
// wire carries codes, never dependency detail).
var (
	// ErrPullRequestNotFound: no PR row exists for the (project, number)
	// pair — an unknown number, or a PR of another project, which
	// reports the same outcome without leaking the foreign entity's
	// existence (docs/45).
	ErrPullRequestNotFound = errors.New("pullrequests: pull request not found")
	// ErrBranchNotFound: a branch of the PR's pair does not exist in the
	// project — unknown, or a branch of another project (same outcome,
	// no foreign existence leak). The external-contribution path does not
	// split this outcome: a source branch in a project that is not the
	// PR's project is only ever usable when it is the creator's own fork
	// of it (00086), and every other foreign source answers exactly this
	// (docs/45).
	ErrBranchNotFound = errors.New("pullrequests: branch not found in the project")
	// ErrBranchNotActive: the source or target branch lifecycle is
	// merged/aborted (docs/43: closed research paths are immutable) — a
	// proposal cannot start from or target a closed path.
	ErrBranchNotActive = errors.New("pullrequests: branch lifecycle is not active")
	// ErrBranchHeadMissing: a branch of the pair has no head state yet —
	// a PR always pins two existing states.
	ErrBranchHeadMissing = errors.New("pullrequests: branch has no head state")
	// ErrValidation: an input fails the domain shape rules (empty
	// identity, an invalid title, an oversized body, source == target,
	// an unknown state value, ...).
	ErrValidation = errors.New("pullrequests: validation failed")
	// ErrStore: the persistence adapter failed (cause kept for the log).
	ErrStore = errors.New("pullrequests: store failure")
)

// Wire codes (docs/45). Outcomes shared with other packages carry the
// same code strings there (BRANCH_NOT_FOUND, BRANCH_NOT_ACTIVE), so one
// outcome has one stable wire name whichever package reports it.
const (
	CodePullRequestNotFound = "PULL_REQUEST_NOT_FOUND"
	CodeBranchNotFound      = "BRANCH_NOT_FOUND"
	CodeBranchNotActive     = "BRANCH_NOT_ACTIVE"
	CodeBranchHeadMissing   = "BRANCH_HEAD_MISSING"
	CodeInvalidTransition   = "PR_INVALID_TRANSITION"
	CodeStateConflict       = "PR_STATE_CONFLICT"
	CodeTerminal            = "PR_TERMINAL"
	CodeValidation          = "VALIDATION_FAILED"
	CodeUnavailable         = "SERVICE_UNAVAILABLE"
)

// TransitionError reports a lifecycle move docs/43 does not allow — the
// machine cannot be skipped (open → merged) and open is not a
// transition target. The same map is enforced by migration 00051 for any
// database write path.
type TransitionError struct {
	// Number is the PR the transition was attempted on.
	Number int64
	// From is the state the PR is in.
	From domain.PullRequestState
	// To is the state the transition was attempted to.
	To domain.PullRequestState
}

// Error implements error.
func (e *TransitionError) Error() string {
	return fmt.Sprintf("pullrequests: PR #%d cannot move %s -> %s (docs/43: open -> review_required -> changes_requested/approved -> merge_ready -> merged; closed/aborted)",
		e.Number, e.From, e.To)
}

// Code is the stable wire code of this outcome (docs/45).
func (e *TransitionError) Code() string { return CodeInvalidTransition }

// StateConflictError reports a transition whose compare-and-swap missed:
// the PR moved between the read and the update (a concurrent transition
// or head refresh landed first). The caller may re-read and retry.
type StateConflictError struct {
	// Number is the PR whose state moved underneath the transition.
	Number int64
	// Current is the state the PR is in now.
	Current domain.PullRequestState
}

// Error implements error.
func (e *StateConflictError) Error() string {
	return fmt.Sprintf("pullrequests: PR #%d state moved underneath the transition (now %s); re-read and retry",
		e.Number, e.Current)
}

// Code is the stable wire code of this outcome (docs/45).
func (e *StateConflictError) Code() string { return CodeStateConflict }

// TerminalError reports an operation on a terminal PR (merged/closed/
// aborted, docs/43): terminal PRs accept no further transitions and no
// head refreshes.
type TerminalError struct {
	// Number is the terminal PR.
	Number int64
	// State is the terminal state it is in.
	State domain.PullRequestState
}

// Error implements error.
func (e *TerminalError) Error() string {
	return fmt.Sprintf("pullrequests: PR #%d is %s — terminal per docs/43, no further transitions and no head refresh",
		e.Number, e.State)
}

// Code is the stable wire code of this outcome (docs/45).
func (e *TerminalError) Code() string { return CodeTerminal }

// BranchNotActiveError reports a creation whose source or target branch
// lifecycle is merged/aborted — a proposal cannot start from or target a
// closed research path (docs/43).
type BranchNotActiveError struct {
	// BranchID is the closed branch.
	BranchID string
	// Lifecycle is the terminal state the branch is in.
	Lifecycle string
}

// Error implements error.
func (e *BranchNotActiveError) Error() string {
	return fmt.Sprintf("pullrequests: branch %s is %s — closed research paths do not accept proposals (docs/43)",
		e.BranchID, e.Lifecycle)
}

// Code is the stable wire code of this outcome (docs/45).
func (e *BranchNotActiveError) Code() string { return CodeBranchNotActive }
