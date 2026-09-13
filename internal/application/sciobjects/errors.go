package sciobjects

import (
	"errors"
	"fmt"
)

// Sentinel errors the service maps for the transport layer (docs/45: the
// wire carries codes, never dependency detail).
var (
	// ErrObjectNotFound: no object row exists for the given id.
	ErrObjectNotFound = errors.New("sciobjects: object not found")
	// ErrVersionNotFound: the object's version log has no row with the
	// given number.
	ErrVersionNotFound = errors.New("sciobjects: version not found")
	// ErrValidation: an input fails the domain shape rules (empty
	// identity, invalid lifecycle state, payload not a JSON object, a
	// next-version request below version 1, ...).
	ErrValidation = errors.New("sciobjects: validation failed")
	// ErrStore: the persistence adapter failed (cause kept for the log).
	ErrStore = errors.New("sciobjects: store failure")
)

// VersionConflictError reports an expected_version compare-and-swap that
// lost: by the time the write ran, the object's current version was Actual,
// not Expected. It is the ONE stable outcome of any concurrent
// next-version creation — two writers with the same expectation always end
// in exactly one success and this error for the rest (docs/45:
// EXPECTED_VERSION_MISMATCH).
type VersionConflictError struct {
	// ObjectID is the object whose version log moved underneath the
	// writer.
	ObjectID string
	// Expected is the version number the writer based its change on.
	Expected int
	// Actual is the current head of the version log, so the caller can
	// re-read and retry with a fresh expectation.
	Actual int
}

// Error implements error. The message carries the two numbers; it never
// leaks storage detail.
func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("sciobjects: expected_version mismatch for object %s: expected version %d, current is %d (re-read the object and retry)",
		e.ObjectID, e.Expected, e.Actual)
}

// Code is the stable wire code of this outcome (docs/45).
func (e *VersionConflictError) Code() string { return CodeVersionConflict }

// Wire codes (docs/45). EXPECTED_VERSION_MISMATCH is the canonical name for
// the version-conflict outcome; OBJECT_NOT_FOUND / OBJECT_VERSION_NOT_FOUND
// follow the USER_NOT_FOUND naming introduced with T0102.
const (
	CodeVersionConflict = "EXPECTED_VERSION_MISMATCH"
	CodeObjectNotFound  = "OBJECT_NOT_FOUND"
	CodeVersionNotFound = "OBJECT_VERSION_NOT_FOUND"
	CodeValidation      = "VALIDATION_FAILED"
	CodeUnavailable     = "SERVICE_UNAVAILABLE"
)
