package relations

import (
	"context"
	"encoding/json"

	"github.com/lichman0405/post/internal/domain"
)

// Repository is the persistence port for typed relations and their
// append-only version log (docs/52). Deliberately ABSENT from the surface:
// any method that could mutate an existing version row. A version is
// written exactly once, at creation; every later change is a new version
// row, so old version content is unchangeable by construction and by
// database trigger (migrations 00014/00015).
//
// Every version pins its endpoints to exact scientific object versions
// (source/target object version ids, docs/07 §3): a write that names a
// non-existent version fails with *ReferencedVersionNotFoundError, never
// stores a dangling edge.
type Repository interface {
	// CreateRelation creates the relation row and its version 1 in one
	// transaction: the two can never be observed apart, and a relation
	// always has exactly one first version. It fails with
	// ErrReferencedVersionNotFound when a source or target object version
	// does not exist (FK), ErrValidation when a referenced project or
	// state does not exist (FK) or an input is malformed, and ErrStore
	// for an adapter failure.
	CreateRelation(ctx context.Context, in CreateRelationParams) (domain.Relation, domain.RelationVersion, error)
	// CreateVersion appends version expected+1 to the relation's log,
	// atomically: the relation's version counter advances from expected
	// to expected+1 only if it still equals expected at write time (the
	// compare-and-swap). Any other interleaving — a concurrent writer
	// won, or the caller's expectation is stale — fails with
	// *VersionConflictError (code EXPECTED_VERSION_MISMATCH), stable
	// across every interleaving. Unknown relations fail with
	// ErrRelationNotFound; a dangling source/target endpoint fails with
	// ErrReferencedVersionNotFound.
	CreateVersion(ctx context.Context, relationID string, expected int, in VersionParams) (domain.RelationVersion, error)
	// GetRelation returns the relation row or ErrRelationNotFound.
	GetRelation(ctx context.Context, relationID string) (domain.Relation, error)
	// GetVersion returns one version row by its 1-based number, or
	// ErrRelationVersionNotFound.
	GetVersion(ctx context.Context, relationID string, versionNo int) (domain.RelationVersion, error)
	// GetLatestVersion returns the head of the version log, or
	// ErrRelationVersionNotFound (a relation without versions is
	// impossible via the port surface).
	GetLatestVersion(ctx context.Context, relationID string) (domain.RelationVersion, error)
	// ListVersions returns the whole version log of the relation in
	// ascending version order; empty for an unknown relation.
	ListVersions(ctx context.Context, relationID string) ([]domain.RelationVersion, error)
	// ListVersionsByType returns every version of the project whose
	// relation_type equals relationType, oldest first. An unknown type
	// yields an empty list: no write through the port can store a type
	// outside the catalog, so there is nothing to return.
	ListVersionsByType(ctx context.Context, projectID string, relationType string) ([]domain.RelationVersion, error)
	// ListVersionsByTypes returns every version of the project whose
	// relation_type is one of relationTypes, oldest first. It is the
	// category query: callers expand a catalog category with
	// relationcatalog.ProvenanceTypes() / DependencyTypes() /
	// TypesOfCategory().
	ListVersionsByTypes(ctx context.Context, projectID string, relationTypes []string) ([]domain.RelationVersion, error)
}

// CreateRelationParams carries the first-version creation request. Version
// holds the content of version 1; the relation row's identity facts sit at
// the top level.
type CreateRelationParams struct {
	ProjectID string
	Version   VersionParams
}

// VersionParams carries one new version's content. Server-authoritative
// fields (CreatedAt, the version number, IntegrityHash) are absent: the
// repository derives them (docs/23 §3 — created_by and the versioning
// facts are never accepted from an untrusted caller; authz belongs to the
// consuming API task).
//
// The canonical schema keeps relation_type and both endpoints on the
// version row (infra/migrations/00006_relations.sql), so every version
// carries its own pins; the service validates each one against the
// catalog.
type VersionParams struct {
	// StateID is the project state this version belongs to (required).
	StateID string
	// RelationType is the edge type; it must be in the V1 relation
	// catalog (internal/rsg/relationcatalog, docs/44).
	RelationType string
	// SourceObjectVersionID and TargetObjectVersionID pin the endpoints
	// to exact scientific object versions (required; edges are
	// version-pinned, docs/07 §3).
	SourceObjectVersionID string
	TargetObjectVersionID string
	// Payload is the edge's scope/metadata as a JSON object (jsonb);
	// empty input is stored as the empty metadata object {}.
	Payload   json.RawMessage
	CreatedBy string
}
