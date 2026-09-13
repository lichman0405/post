package branches

import (
	"errors"
	"fmt"
)

// Sentinel errors the service maps for the transport layer (docs/45: the
// wire carries codes, never dependency detail).
var (
	// ErrBranchNotFound: no branch row exists for the (project, branch)
	// pair — an unknown id, or a branch of another project, which reports
	// the same outcome without leaking the foreign entity's existence
	// (docs/45).
	ErrBranchNotFound = errors.New("branches: branch not found")
	// ErrBranchNameTaken: the project already has a branch with this name
	// (UNIQUE(project_id, name)).
	ErrBranchNameTaken = errors.New("branches: branch name already taken")
	// ErrBaseStateNotFound: the requested base state does not exist in the
	// project — either the state does not exist at all, or it belongs to
	// another project (same outcome, no foreign existence leak).
	ErrBaseStateNotFound = errors.New("branches: base state does not exist in the project")
	// ErrBranchNotActive: the branch lifecycle is merged or aborted —
	// terminal per docs/43, so the operation (a lifecycle transition on a
	// closed branch, or a commit, which the states package reports through
	// its own BranchNotActiveError) cannot apply.
	ErrBranchNotActive = errors.New("branches: branch lifecycle is not active")
	// ErrMainProtected: the operation does not apply to the project's main
	// branch — main is the canonical integration branch, its lifecycle is
	// the project's, not one research path's (docs/09 §3); frozen-main
	// write protection itself is T0601's gate.
	ErrMainProtected = errors.New("branches: the main branch is protected")
	// ErrPublicBranchInPrivateProject: a private project cannot host a
	// public branch — visibility widening beyond the project preset goes
	// through the explicit, audited publication flow, never through branch
	// creation (docs/12 §3).
	ErrPublicBranchInPrivateProject = errors.New("branches: a private project cannot host a public branch")
	// ErrStateNotFound: the branch exists but has no head state yet (a
	// branch seeded outside the domain service without a base state).
	ErrStateNotFound = errors.New("branches: branch has no head state")
	// ErrValidation: an input fails the domain shape rules (empty
	// identity, an invalid branch name, an unknown visibility value, a
	// blank or oversized purpose, ...).
	ErrValidation = errors.New("branches: validation failed")
	// ErrStore: the persistence adapter failed (cause kept for the log).
	ErrStore = errors.New("branches: store failure")
)

// Wire codes (docs/45). Outcomes shared with the states package carry the
// same code strings there (BRANCH_NOT_FOUND, STATE_NOT_FOUND,
// BRANCH_NOT_ACTIVE), so one outcome has one stable wire name whichever
// package reports it.
const (
	CodeBranchNotFound               = "BRANCH_NOT_FOUND"
	CodeBranchNameTaken              = "BRANCH_NAME_TAKEN"
	CodeBaseStateNotFound            = "BASE_STATE_NOT_FOUND"
	CodeBranchNotActive              = "BRANCH_NOT_ACTIVE"
	CodeMainProtected                = "MAIN_BRANCH_PROTECTED"
	CodePublicBranchInPrivateProject = "PUBLIC_BRANCH_IN_PRIVATE_PROJECT"
	CodeStateNotFound                = "STATE_NOT_FOUND"
	CodeValidation                   = "VALIDATION_FAILED"
	CodeUnavailable                  = "SERVICE_UNAVAILABLE"
)

// NotActiveError reports a lifecycle transition or commit attempted on a
// branch whose lifecycle is already terminal (merged/aborted, docs/43) —
// the branch's history is immutable once its research path closed.
type NotActiveError struct {
	// BranchID is the branch that is no longer active.
	BranchID string
	// Lifecycle is the terminal state the branch is in.
	Lifecycle string
}

// Error implements error.
func (e *NotActiveError) Error() string {
	return fmt.Sprintf("branches: branch %s is %s — merged/aborted history is immutable (docs/43), no commits and no further lifecycle transitions",
		e.BranchID, e.Lifecycle)
}

// Code is the stable wire code of this outcome (docs/45).
func (e *NotActiveError) Code() string { return CodeBranchNotActive }
