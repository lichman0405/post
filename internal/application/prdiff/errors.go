package prdiff

import "errors"

// Sentinel errors the service maps for its consumers (docs/45: stable
// outcomes, never dependency detail).
var (
	// ErrPullRequestNotFound: no PR row exists for the number in this
	// project (also for a PR of another project — never leak a foreign
	// entity's existence).
	ErrPullRequestNotFound = errors.New("prdiff: pull request not found")
	// ErrStateNotFound: one of the three pinned states cannot be resolved
	// — a PR whose proposed head, base or target head names no state row.
	// It is a data outcome, not an adapter failure.
	ErrStateNotFound = errors.New("prdiff: state not found")
	// ErrValidation: the request's shape fails (empty project id, a
	// non-positive number).
	ErrValidation = errors.New("prdiff: validation failed")
	// ErrStore: an adapter failed, or the stored rows cannot render a
	// diff (cause kept for the log).
	ErrStore = errors.New("prdiff: store failure")
)

// Stable error codes for the wire (docs/45). The vocabulary is the one
// the sibling PR read surfaces already speak: the PR code from
// internal/application/pullrequests, this package's own code for the
// unresolvable-state outcome (the diff compares state rows, and one of
// them is missing).
const (
	// CodeStateNotFound: the wire code of ErrStateNotFound.
	CodeStateNotFound = "STATE_NOT_FOUND"
)
