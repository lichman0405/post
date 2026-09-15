package reviews

import "errors"

// Sentinel errors the service maps for the transport layer (docs/45: the
// wire carries codes, never dependency detail).
var (
	// ErrValidation: an input fails the domain shape rules (unknown kind
	// or decision, oversized body, malformed identity, ...).
	ErrValidation = errors.New("reviews: validation failed")
	// ErrForbidden: the authorization engine refused the submission, or
	// the conditional verdict could not be resolved in the actor's favor
	// (docs/12: default deny — an unresolved condition is refusal).
	ErrForbidden = errors.New("reviews: forbidden")
	// ErrAlreadyReviewed: this reviewer already recorded a decision for
	// this dimension on this head — a review of a state is final; the
	// machine's path forward is the author updating the head (docs/43).
	ErrAlreadyReviewed = errors.New("reviews: reviewer already recorded this kind for this head")
	// ErrStore: the persistence adapter or a required gate failed (cause
	// kept for the log).
	ErrStore = errors.New("reviews: store failure")
)

// Wire codes (docs/45). Outcomes shared with other packages carry the
// same code strings there (PULL_REQUEST_NOT_FOUND, PR_TERMINAL,
// AUTH_FORBIDDEN, VALIDATION_FAILED), so one outcome has one stable wire
// name whichever package reports it.
const (
	CodeAlreadyReviewed = "REVIEW_ALREADY_SUBMITTED"
	CodeForbidden       = "AUTH_FORBIDDEN"
	CodeValidation      = "VALIDATION_FAILED"
	CodeUnavailable     = "SERVICE_UNAVAILABLE"
)
