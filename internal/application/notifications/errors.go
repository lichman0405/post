package notifications

import "errors"

// Sentinel errors the service maps to wire codes. Callers test with
// errors.Is — never by string comparison.
var (
	// ErrValidation: malformed input (an unknown cadence, an empty one).
	ErrValidation = errors.New("notifications: invalid input")
	// ErrStore: the store itself failed (dependency down, driver error).
	ErrStore = errors.New("notifications: store failure")
)

// Codes shared with the rest of the API (the orgs/subscriptions
// vocabulary).
const (
	// CodeValidationFailed answers malformed input.
	CodeValidationFailed = "VALIDATION_FAILED"
	// CodeServiceUnavailable answers a failed store.
	CodeServiceUnavailable = "SERVICE_UNAVAILABLE"
)
