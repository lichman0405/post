package sciobjects

import (
	"context"
	"encoding/json"

	"github.com/lichman0405/post/internal/domain"
)

// Repository is the persistence port for scientific objects and their
// append-only version log (docs/52: application orchestrates against ports;
// adapters live in internal/persistence). Deliberately ABSENT from the
// surface: any method that could mutate an existing version row. A version
// is written exactly once, at creation; every later change is a new version
// row, so old version content is unchangeable by construction and by
// database trigger (migrations 00014/00015).
type Repository interface {
	// CreateObject creates the object row and its version 1 in one
	// transaction: the two can never be observed apart, and an object
	// always has exactly one first version. It fails with ErrValidation
	// when a referenced project or state does not exist (FK), and
	// ErrStore for an adapter failure.
	CreateObject(ctx context.Context, in CreateObjectParams) (domain.ScientificObject, domain.ScientificObjectVersion, error)
	// CreateVersion appends version expected+1 to the object's log,
	// atomically: the object's version counter advances from expected to
	// expected+1 only if it still equals expected at write time (the
	// compare-and-swap). Any other interleaving — a concurrent writer
	// won, or the caller's expectation is stale — fails with
	// *VersionConflictError (code EXPECTED_VERSION_MISMATCH), stable
	// across every interleaving. Unknown objects fail with
	// ErrObjectNotFound.
	CreateVersion(ctx context.Context, objectID string, expected int, in VersionParams) (domain.ScientificObjectVersion, error)
	// GetObject returns the object row or ErrObjectNotFound.
	GetObject(ctx context.Context, objectID string) (domain.ScientificObject, error)
	// GetVersion returns one version row by its 1-based number, or
	// ErrVersionNotFound.
	GetVersion(ctx context.Context, objectID string, versionNo int) (domain.ScientificObjectVersion, error)
	// GetLatestVersion returns the head of the version log, or
	// ErrVersionNotFound (an object without versions is impossible via
	// the port surface).
	GetLatestVersion(ctx context.Context, objectID string) (domain.ScientificObjectVersion, error)
	// ListVersions returns the whole version log of the object in
	// ascending version order; empty for an unknown object.
	ListVersions(ctx context.Context, objectID string) ([]domain.ScientificObjectVersion, error)
}

// CreateObjectParams carries the first-version creation request. Version
// holds the content of version 1; the object row's identity facts sit at
// the top level.
type CreateObjectParams struct {
	ProjectID  string
	ObjectType string
	CreatedBy  string
	Version    VersionParams
}

// VersionParams carries one new version's content. Server-authoritative
// fields (CreatedAt, the version number, IntegrityHash) are absent: the
// repository derives them (docs/23 §3 — created_by and the versioning
// facts are never accepted from an untrusted caller; authz belongs to the
// consuming API task).
type VersionParams struct {
	// StateID is the project state this version belongs to (required).
	StateID string
	// BranchID is the research branch this version is created on; nil
	// when the version predates branch resolution.
	BranchID *string
	// SchemaID and SchemaVersion pin the JSON Schema governing Payload
	// (the schemareg.Ref{ID,Version} halves).
	SchemaID       string
	SchemaVersion  string
	Title          string
	LifecycleState domain.LifecycleState
	// Payload is the versioned scientific content as a JSON object
	// (jsonb).
	Payload json.RawMessage
	// VisibilityPolicyID pins the rights policy; nil inherits the
	// project default.
	VisibilityPolicyID *string
	CreatedBy          string
}
