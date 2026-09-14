package states

import (
	"errors"
	"fmt"
)

// Sentinel errors the service maps for the transport layer (docs/45: the
// wire carries codes, never dependency detail).
var (
	// ErrStateNotFound: no project state row exists for the given id (or
	// the branch has no head state yet).
	ErrStateNotFound = errors.New("states: state not found")
	// ErrCommitNotFound: no state commit row exists for the given id.
	ErrCommitNotFound = errors.New("states: commit not found")
	// ErrBranchNotFound: the commit names a branch that does not exist in
	// the project (an existing branch of another project reports the same
	// outcome — never leak another project's entity existence).
	ErrBranchNotFound = errors.New("states: branch not found")
	// ErrStateExists: a state with the same content hash already exists
	// in the project (states are content-addressed; UNIQUE(project_id,
	// state_hash)). The identical transition was already committed — read
	// it back with GetStateByHash instead of committing twice.
	ErrStateExists = errors.New("states: state already exists")
	// ErrValidation: an input fails the domain shape rules (empty
	// identity, unknown via channel, unknown operation kind, an empty
	// message or operation list, ...).
	ErrValidation = errors.New("states: validation failed")
	// ErrStore: the persistence adapter failed (cause kept for the log).
	ErrStore = errors.New("states: store failure")
)

// StateConflictError reports a commit whose expected base state lost the
// compare-and-swap: by the time the transaction ran, the branch head was
// Actual, not Expected. It is the ONE stable outcome of any concurrent
// commit built on a superseded head — the branch chain stays linear, and
// the losing transaction wrote nothing (docs/45: BRANCH_STATE_CONFLICT).
type StateConflictError struct {
	// BranchID is the branch whose head moved underneath the writer.
	BranchID string
	// Expected is the base state the commit was built on; nil when the
	// writer expected the branch to have no head yet.
	Expected *string
	// Actual is the current branch head, so the caller can re-read and
	// retry with a fresh base.
	Actual *string
}

// Error implements error.
func (e *StateConflictError) Error() string {
	return fmt.Sprintf("states: branch state conflict for branch %s: expected base %v, current head is %v (re-read the branch head and retry)",
		e.BranchID, e.Expected, e.Actual)
}

// Code is the stable wire code of this outcome (docs/45).
func (e *StateConflictError) Code() string { return CodeBranchStateConflict }

// BranchNotActiveError reports a commit attempted on a branch whose
// lifecycle is merged or aborted — closed research paths are immutable
// (docs/43: "merged/aborted history immutable"), so no state transition
// can land on them. The same stable outcome as the branches package's
// *branches.NotActiveError for lifecycle transitions; both carry the
// BRANCH_NOT_ACTIVE wire code (T0205).
type BranchNotActiveError struct {
	// BranchID is the branch that no longer accepts commits.
	BranchID string
	// Lifecycle is the terminal state the branch is in.
	Lifecycle string
}

// Error implements error.
func (e *BranchNotActiveError) Error() string {
	return fmt.Sprintf("states: branch %s is %s — merged/aborted history is immutable (docs/43), no commits land on it",
		e.BranchID, e.Lifecycle)
}

// Code is the stable wire code of this outcome (docs/45).
func (e *BranchNotActiveError) Code() string { return CodeBranchNotActive }

// CommitWriteError wraps a failure raised by the commit's operation
// callback — the semantic writes that ran inside the commit transaction.
// The adapter wraps callback errors in it so the service can tell them
// apart from adapter failures: the callback's error is the domain outcome
// of the semantic write and passes through unchanged (with its own wire
// code); an adapter failure is reported as ErrStore.
type CommitWriteError struct {
	// Err is the callback's error.
	Err error
}

// Error implements error.
func (e *CommitWriteError) Error() string {
	return fmt.Sprintf("states: commit operations failed: %v", e.Err)
}

// Unwrap returns the wrapped callback error.
func (e *CommitWriteError) Unwrap() error { return e.Err }

// Wire codes (docs/45). BRANCH_STATE_CONFLICT is the canonical name for the
// base-state compare-and-swap outcome; STATE_ALREADY_EXISTS reports the
// content-address collision of an identical transition; the rest follow the
// T0202/T0203 naming shape.
const (
	CodeBranchStateConflict = "BRANCH_STATE_CONFLICT"
	CodeStateNotFound       = "STATE_NOT_FOUND"
	CodeCommitNotFound      = "STATE_COMMIT_NOT_FOUND"
	CodeBranchNotFound      = "BRANCH_NOT_FOUND"
	CodeBranchNotActive     = "BRANCH_NOT_ACTIVE"
	CodeStateExists         = "STATE_ALREADY_EXISTS"
	CodeValidation          = "VALIDATION_FAILED"
	CodeUnavailable         = "SERVICE_UNAVAILABLE"
)
