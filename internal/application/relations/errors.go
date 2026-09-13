package relations

import (
	"errors"
	"fmt"
)

// Sentinel errors the service maps for the transport layer (docs/45: the
// wire carries codes, never dependency detail).
var (
	// ErrRelationNotFound: no relation row exists for the given id.
	ErrRelationNotFound = errors.New("relations: relation not found")
	// ErrRelationVersionNotFound: the relation's version log has no row
	// with the given number.
	ErrRelationVersionNotFound = errors.New("relations: relation version not found")
	// ErrReferencedVersionNotFound: the source or target endpoint names a
	// scientific object version that does not exist — edges are
	// version-pinned, so a dangling endpoint is rejected, never stored.
	ErrReferencedVersionNotFound = errors.New("relations: referenced object version not found")
	// ErrValidation: an input fails the domain shape rules (empty
	// identity, payload not a JSON object, a next-version request below
	// version 1, ...).
	ErrValidation = errors.New("relations: validation failed")
	// ErrStore: the persistence adapter failed (cause kept for the log).
	ErrStore = errors.New("relations: store failure")
)

// UnknownRelationTypeError reports a relation_type that is not in the
// relation catalog (docs/44). The catalog is the only V1 type source:
// namespaced extension types have no declaration mechanism yet, so they
// fail explicitly here rather than silently entering strong inference
// (docs/44: a custom relation without a declaration must not participate
// in strong reasoning).
type UnknownRelationTypeError struct {
	// Type is the relation_type the caller asked for.
	Type string
}

// Error implements error. The message carries the rejected type and never
// leaks storage detail.
func (e *UnknownRelationTypeError) Error() string {
	return fmt.Sprintf("relations: relation type %q is not in the V1 catalog (docs/44): register a declared type or use a canonical core type", e.Type)
}

// Code is the stable wire code of this outcome (docs/45).
func (e *UnknownRelationTypeError) Code() string { return CodeRSGValidationFailed }

// ReferencedVersionNotFoundError reports which endpoint of a write named a
// scientific object version that does not exist. The write is rejected
// whole: a relation version is never stored with a dangling endpoint.
type ReferencedVersionNotFoundError struct {
	// Side is "source" or "target".
	Side string
	// VersionID is the object version id that does not exist.
	VersionID string
}

// Error implements error.
func (e *ReferencedVersionNotFoundError) Error() string {
	return fmt.Sprintf("relations: %s object version %s does not exist (edges are version-pinned; the referenced version must exist)", e.Side, e.VersionID)
}

// Code is the stable wire code of this outcome (docs/45).
func (e *ReferencedVersionNotFoundError) Code() string { return CodeObjectVersionNotFound }

// VersionConflictError reports an expected_version compare-and-swap that
// lost: by the time the write ran, the relation's current version was
// Actual, not Expected. It is the ONE stable outcome of any concurrent
// next-version creation — two writers with the same expectation always end
// in exactly one success and this error for the rest (docs/45:
// EXPECTED_VERSION_MISMATCH).
type VersionConflictError struct {
	// RelationID is the relation whose version log moved underneath the
	// writer.
	RelationID string
	// Expected is the version number the writer based its change on.
	Expected int
	// Actual is the current head of the version log, so the caller can
	// re-read and retry with a fresh expectation.
	Actual int
}

// Error implements error.
func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("relations: expected_version mismatch for relation %s: expected version %d, current is %d (re-read the relation and retry)",
		e.RelationID, e.Expected, e.Actual)
}

// Code is the stable wire code of this outcome (docs/45).
func (e *VersionConflictError) Code() string { return CodeVersionConflict }

// Wire codes (docs/45). EXPECTED_VERSION_MISMATCH is the canonical name for
// the version-conflict outcome; OBJECT_VERSION_NOT_FOUND is the code
// introduced with T0202 for a missing object version and reused here for a
// dangling relation endpoint; RSG_VALIDATION_FAILED covers inputs that
// violate the RSG shape (docs/45).
const (
	CodeVersionConflict         = "EXPECTED_VERSION_MISMATCH"
	CodeRelationNotFound        = "RELATION_NOT_FOUND"
	CodeRelationVersionNotFound = "RELATION_VERSION_NOT_FOUND"
	CodeObjectVersionNotFound   = "OBJECT_VERSION_NOT_FOUND"
	CodeValidation              = "VALIDATION_FAILED"
	CodeRSGValidationFailed     = "RSG_VALIDATION_FAILED"
	CodeUnavailable             = "SERVICE_UNAVAILABLE"
)
