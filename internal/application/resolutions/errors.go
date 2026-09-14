package resolutions

import "errors"

// Sentinel errors the resolution service produces itself (docs/45: the
// wire carries codes, never dependency detail). Outcomes owned by the
// underlying services — a missing state, an unreadable snapshot, a
// refused membership — pass through unchanged so the transport maps each
// outcome once, wherever it arose.
var (
	// ErrForbidden: the authorization engine refused the action (a denied
	// matrix verdict). The caller is refused before any conflict report is
	// computed, so the refusal never discloses whether the states exist.
	ErrForbidden = errors.New("resolutions: forbidden")
	// ErrValidation: an input fails the domain shape rules (an unknown
	// decision kind, an empty triple, a decision that names no target).
	ErrValidation = errors.New("resolutions: validation failed")
	// ErrConflictNotFound: a decision names a conflict the detector did
	// not classify for this triple — the plan would contain a decision
	// about a conflict that does not exist (stale UI, or a forged input).
	ErrConflictNotFound = errors.New("resolutions: conflict not found")
	// ErrStore: an adapter or policy engine failed (cause kept for the log).
	ErrStore = errors.New("resolutions: store failure")
)

// Wire codes (docs/45). The shared outcomes reuse the canonical code
// strings of the owning packages (projects, states, diffs); the two codes
// below are this package's own.
const (
	CodeValidation       = "VALIDATION_FAILED"
	CodeConflictNotFound = "CONFLICT_NOT_FOUND"
	// CodeForbidden matches the canonical matrix-refusal string (docs/45).
	CodeForbidden = "AUTH_FORBIDDEN"
	CodeStore     = "SERVICE_UNAVAILABLE"
)
