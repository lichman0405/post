package manifests

import "errors"

// Sentinel errors the service maps for its consumers (docs/45: stable
// outcomes, never dependency detail).
var (
	// ErrStateNotFound: no project state row exists for the given id.
	ErrStateNotFound = errors.New("manifests: state not found")
	// ErrValidation: an input fails the domain shape rules (empty state
	// id).
	ErrValidation = errors.New("manifests: validation failed")
	// ErrStore: the persistence adapter failed, or the snapshot it
	// returned cannot be rendered (cause kept for the log).
	ErrStore = errors.New("manifests: store failure")
)
