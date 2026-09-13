package audit

import "errors"

// Error codes on the wire (docs/45): every activity failure maps to one of
// these stable codes, rendered inside the standard error envelope. The
// codes for denied reads reuse the owning surface's code (PROJECT_NOT_FOUND
// / ORG_NOT_FOUND) via that surface's own sentinel errors.
const (
	// CodeValidationFailed is returned for a malformed cursor or limit.
	CodeValidationFailed = "VALIDATION_FAILED"
	// CodeServiceUnavailable is returned when the audit store failed.
	CodeServiceUnavailable = "SERVICE_UNAVAILABLE"
)

// Sentinel errors the service maps to wire codes. Callers test with
// errors.Is — never by string comparison.
var (
	// ErrValidation: malformed cursor or limit input.
	ErrValidation = errors.New("audit: invalid input")
	// ErrStore: the store itself failed (dependency down, driver error).
	ErrStore = errors.New("audit: store failure")
)
