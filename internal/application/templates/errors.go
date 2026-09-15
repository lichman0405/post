package templates

import "errors"

// Sentinel errors the service maps for the transport layer (docs/45: the
// wire carries codes, never dependency detail).
var (
	// ErrTemplateNotFound: no template exists with this id (any version),
	// or the requested version of the id does not exist. The two answer
	// the same error — the catalog discloses nothing a caller could
	// enumerate.
	ErrTemplateNotFound = errors.New("templates: template not found")
	// ErrValidation: an input fails the template shape rules (template
	// id/version labels, or the project fields the instantiation must
	// validate before the projects service runs).
	ErrValidation = errors.New("templates: validation failed")
	// ErrStore: a dependency failed (cause kept for the log).
	ErrStore = errors.New("templates: store failure")
	// ErrInstantiationExists: the project already records a template
	// origin — one project has at most one (UNIQUE (project_id)); a
	// second record is a store-level race, refused here.
	ErrInstantiationExists = errors.New("templates: project already records a template instantiation")
	// ErrInstantiationNotFound: the project records no template origin.
	ErrInstantiationNotFound = errors.New("templates: no template instantiation recorded")
)

// Wire codes (docs/45).
const (
	CodeTemplateNotFound = "TEMPLATE_NOT_FOUND"
	CodeValidation       = "TEMPLATE_VALIDATION_FAILED"
	CodeUnavailable      = "SERVICE_UNAVAILABLE"
)
