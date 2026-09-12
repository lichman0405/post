package profile

import "errors"

// Stable errors the service maps for the transport layer (docs/45: the
// wire carries codes, never dependency detail).
var (
	// ErrNotFound: no profile exists for the given id/handle.
	ErrNotFound = errors.New("profile: not found")
	// ErrForbidden: the actor is not allowed to apply this update — only
	// the profile owner may edit the editable fields.
	ErrForbidden = errors.New("profile: forbidden")
	// ErrValidation: a provided field fails the domain shape rules.
	ErrValidation = errors.New("profile: validation failed")
	// ErrStore: the persistence adapter failed (cause kept for the log).
	ErrStore = errors.New("profile: store failure")
	// ErrHandleTaken: the requested handle belongs to another account.
	ErrHandleTaken = errors.New("profile: handle already taken")
)

// Wire codes (docs/45). AUTH_FORBIDDEN, VALIDATION_FAILED and
// SERVICE_UNAVAILABLE keep the strings the rest of the API already uses
// (docs/45 names AUTH_FORBIDDEN first-class); USER_NOT_FOUND and
// HANDLE_ALREADY_TAKEN are new with T0102.
const (
	CodeUserNotFound = "USER_NOT_FOUND"
	CodeForbidden    = "AUTH_FORBIDDEN"
	CodeValidation   = "VALIDATION_FAILED"
	CodeUnavailable  = "SERVICE_UNAVAILABLE"
	CodeHandleTaken  = "HANDLE_ALREADY_TAKEN"
)
