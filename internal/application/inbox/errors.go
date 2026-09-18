package inbox

import "errors"

// Sentinel errors the service maps to wire codes. Callers test with
// errors.Is — never by string comparison.
var (
	// ErrNotFound: a mark-read anchor that is not the actor's, does not
	// exist, or is not a delivered web row. The three answer identically,
	// the existence-hiding rule the subscription reads keep.
	ErrNotFound = errors.New("inbox: not found")
	// ErrValidation: malformed input (view name, page size, anchor shape,
	// anchor count).
	ErrValidation = errors.New("inbox: invalid input")
	// ErrStore: the store itself failed (dependency down, driver error).
	ErrStore = errors.New("inbox: store failure")
)

// Codes shared with the rest of the API (VALIDATION_FAILED and
// SERVICE_UNAVAILABLE follow the orgs/webhook vocabulary; the inbox's own
// code is new with T1003).
const (
	// CodeNotFound answers a mark-read anchor the actor cannot mark.
	CodeNotFound = "INBOX_ENTRY_NOT_FOUND"
	// CodeValidationFailed answers malformed input.
	CodeValidationFailed = "VALIDATION_FAILED"
	// CodeServiceUnavailable answers a failed store.
	CodeServiceUnavailable = "SERVICE_UNAVAILABLE"
)
