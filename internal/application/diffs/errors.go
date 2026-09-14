package diffs

import "errors"

// Sentinel errors the service maps for its consumers (docs/45: stable
// outcomes, never dependency detail).
var (
	// ErrStateNotFound: no project state row exists for one of the three
	// given ids.
	ErrStateNotFound = errors.New("diffs: state not found")
	// ErrValidation: an input fails the domain shape rules (empty ids, or
	// a state that does not belong to the given project).
	ErrValidation = errors.New("diffs: validation failed")
	// ErrStore: the persistence adapter failed, or a snapshot it returned
	// cannot be rendered (cause kept for the log).
	ErrStore = errors.New("diffs: store failure")
)
