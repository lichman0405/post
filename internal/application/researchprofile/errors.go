package researchprofile

import "errors"

// Stable errors the service maps for the transport layer (docs/45: the wire
// carries codes, never dependency detail).
var (
	// ErrNotFound: no public profile exists under the given id or slug. It
	// is ONE error for three situations on purpose — unknown id, an account
	// that is disabled, an organization that is deactivated — because the
	// transport must not be able to tell them apart: "this id exists but is
	// not public" is exactly the fact a public API may not state (docs/45
	// existence hiding, the same rule the project read applies to a private
	// project).
	ErrNotFound = errors.New("researchprofile: not found")
	// ErrStore: the persistence adapter failed. The cause is kept for the
	// log and never reaches the wire.
	ErrStore = errors.New("researchprofile: store failure")
)

// Wire codes (docs/45). USER_NOT_FOUND keeps the string the profile surface
// already answers with (cmd/api/profilehttp); ORG_NOT_FOUND follows the
// ORG_SLUG_TAKEN naming the organization surface already uses; and
// SERVICE_UNAVAILABLE is the platform's shared "not right now" code.
const (
	CodeUserNotFound = "USER_NOT_FOUND"
	CodeOrgNotFound  = "ORG_NOT_FOUND"
	CodeUnavailable  = "SERVICE_UNAVAILABLE"
)
