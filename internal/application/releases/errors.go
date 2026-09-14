package releases

import "errors"

// Sentinel errors the service maps for its consumers (docs/45: stable
// outcomes, never dependency detail).
var (
	// ErrValidation: an input fails the domain shape rules (empty ids,
	// a release version outside the bounded-label shape).
	ErrValidation = errors.New("releases: validation failed")
	// ErrStateNotFound: no state row exists for the given id, or the
	// state belongs to another project (existence-hiding — never leak a
	// foreign entity, docs/45).
	ErrStateNotFound = errors.New("releases: state not found")
	// ErrProjectNotFound: no project row exists for the given id.
	ErrProjectNotFound = errors.New("releases: project not found")
	// ErrNotMainState: the state exists but is not an accepted main
	// snapshot — a release manifests only accepted main state
	// (docs/11 §1).
	ErrNotMainState = errors.New("releases: state is not an accepted main snapshot")
	// ErrPolicyNotFound: a named policy version does not exist, or does
	// not belong to the release's organization/project scope.
	ErrPolicyNotFound = errors.New("releases: policy version not found")
	// ErrStore: a persistence adapter failed, or the data it returned
	// cannot be rendered (cause kept for the log).
	ErrStore = errors.New("releases: store failure")
)
