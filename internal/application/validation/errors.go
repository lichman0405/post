package validation

import "errors"

// Sentinel errors the service maps for the transport layer (docs/45: the
// wire carries codes, never dependency detail). Deliberately local to this
// package: the states package (which imports this one) keeps its own
// sentinels, and the persistence adapters translate between the two.
var (
	// ErrBranchNotFound: the branch does not exist in the given project —
	// or it exists in another project, which reports the same outcome
	// (never leak another project's entity existence).
	ErrBranchNotFound = errors.New("validation: branch not found")
	// ErrStateNotFound: the branch has no head state yet (an empty
	// branch); reads that cannot name a chain are empty, not errors.
	ErrStateNotFound = errors.New("validation: state not found")
	// ErrValidation: an input fails the domain shape rules (unknown gate,
	// missing ids, a gate that cannot guard a commit).
	ErrValidation = errors.New("validation: validation failed")
	// ErrStore: the persistence adapter failed (cause kept for the log).
	ErrStore = errors.New("validation: store failure")
)

// Wire codes (docs/45).
const (
	CodeBranchNotFound = "BRANCH_NOT_FOUND"
	CodeValidation     = "VALIDATION_FAILED"
	CodeUnavailable    = "SERVICE_UNAVAILABLE"
)
