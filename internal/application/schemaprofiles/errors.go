package schemaprofiles

import "errors"

// Sentinel errors the service maps for the transport layer (docs/45: the
// wire carries codes, never dependency detail).
var (
	// ErrProfileNotFound: no profile version row exists for the given
	// (project, schema id, version).
	ErrProfileNotFound = errors.New("schemaprofiles: schema profile not found")
	// ErrProfileVersionExists: a profile version with this id+version is
	// already registered (with the same or different content — versions
	// are immutable, new content takes a new version).
	ErrProfileVersionExists = errors.New("schemaprofiles: schema profile version already registered")
	// ErrForbidden: the actor may not register profiles in this project —
	// "not a member" and "role too low" answer the same error, so the
	// check discloses nothing (docs/45).
	ErrForbidden = errors.New("schemaprofiles: forbidden")
	// ErrProjectNotFound: the project is unknown or not visible to the
	// actor (existence hiding).
	ErrProjectNotFound = errors.New("schemaprofiles: project not found")
	// ErrValidation: an input fails the profile shape rules (name,
	// version label, base pin, custom field definitions).
	ErrValidation = errors.New("schemaprofiles: validation failed")
	// ErrStore: the persistence adapter or the registry failed (cause
	// kept for the log). Unreachable dependencies fall here — the
	// startup loader treats ErrStore as retryable.
	ErrStore = errors.New("schemaprofiles: store failure")
	// ErrCorruption: a persisted profile row is corrupted — its content
	// does not re-hash to its stored content hash, or it conflicts with
	// the registry's immutable content at the same ref, or it does not
	// compile as a JSON Schema. Corruption is NOT retryable: the API
	// exits (fail fast) rather than serving a registry that diverges
	// from the database rows (docs/21 §10).
	ErrCorruption = errors.New("schemaprofiles: persisted profile content is corrupted")
)

// Wire codes (docs/45).
const (
	CodeProfileNotFound      = "SCHEMA_PROFILE_NOT_FOUND"
	CodeProfileVersionExists = "SCHEMA_PROFILE_VERSION_EXISTS"
	CodeForbidden            = "SCHEMA_PROFILE_FORBIDDEN"
	CodeValidation           = "SCHEMA_PROFILE_VALIDATION_FAILED"
	CodeUnavailable          = "SERVICE_UNAVAILABLE"
)
