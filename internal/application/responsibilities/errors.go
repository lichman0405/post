package responsibilities

import "errors"

// Sentinel errors the service maps to wire codes. Callers test with
// errors.Is — never by string comparison.
var (
	// ErrProjectNotFound: the project does not exist — or exists but the
	// caller may not see it (read existence hiding, docs/45; the same
	// answer the projects surface gives).
	ErrProjectNotFound = errors.New("responsibilities: project not found")
	// ErrForbidden: the actor lacks the governance role for the action
	// (rule and assignment writes are owner-only; "not a member" and
	// "role too low" answer the same thing, so the check discloses
	// nothing).
	ErrForbidden = errors.New("responsibilities: not allowed")
	// ErrUserNotFound: the user named by an assignment does not exist.
	ErrUserNotFound = errors.New("responsibilities: user not found")
	// ErrRuleExists: the project already routes this (match_kind,
	// match_value) to this responsibility (migration 00084's uniqueness:
	// the same mapping twice is the same requirement twice).
	ErrRuleExists = errors.New("responsibilities: rule already exists")
	// ErrValidation: malformed input (unknown match kind, blank or
	// oversized label, blank or untrimmed match value).
	ErrValidation = errors.New("responsibilities: invalid input")
	// ErrStore: the store itself failed (dependency down, driver error).
	ErrStore = errors.New("responsibilities: store failure")
)
