package rsg

import "errors"

// Sentinel errors the RSG service produces itself (docs/45: the wire
// carries codes, never dependency detail). Outcomes owned by the
// underlying services — a missing object, a lost version compare-and-swap,
// a blocked validation gate, a branch state conflict — pass through
// unchanged (their packages define the canonical sentinels and wire
// codes), so the transport maps each outcome once, wherever it arose.
var (
	// ErrForbidden: the authorization engine refused the action (a denied
	// matrix verdict, or a conditional form this site does not resolve).
	// The caller is refused before any object/relation lookup, so the
	// refusal never discloses whether the target exists (docs/45).
	ErrForbidden = errors.New("rsg: forbidden")
	// ErrValidation: an input fails the domain shape rules (an unknown
	// object type, a payload that is not a JSON object, an expected_version
	// below 1, a missing endpoint, ...).
	ErrValidation = errors.New("rsg: validation failed")
	// ErrStore: an adapter or policy engine failed (cause kept for the log).
	ErrStore = errors.New("rsg: store failure")
)

// Wire codes (docs/45). The shared outcomes reuse the canonical code
// strings of the owning packages (projects, branches, states, sciobjects,
// relations, rsg/validation); the two codes below are this package's own.
const (
	CodeValidation  = "VALIDATION_FAILED"
	CodeUnavailable = "SERVICE_UNAVAILABLE"
	// CodeForbidden matches the profile surface's AUTH_FORBIDDEN string
	// (docs/45): one stable code for every matrix refusal.
	CodeForbidden = "AUTH_FORBIDDEN"
)
