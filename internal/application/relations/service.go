package relations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/relationcatalog"
)

// Service orchestrates the typed relation use cases against the Repository
// port. It owns input validation (the port trusts, the service verifies) —
// including relation type catalog validation — and maps store outcomes onto
// the package sentinels; it does NOT authorize — actors and project
// membership checks belong to the consuming API task, which passes only
// resolved identities in.
type Service struct {
	repo Repository
}

// NewService wires the service.
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// CreateRelation creates the relation together with its version 1 — one
// atomic store call, so the pair can never be observed apart. The type is
// validated against the V1 relation catalog and both endpoints must name
// existing scientific object versions.
func (s *Service) CreateRelation(ctx context.Context, in CreateRelationParams) (domain.Relation, domain.RelationVersion, error) {
	if in.ProjectID == "" {
		return domain.Relation{}, domain.RelationVersion{}, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	v, err := normalizeVersionParams(in.Version)
	if err != nil {
		return domain.Relation{}, domain.RelationVersion{}, err
	}
	in.Version = v
	rel, v1, err := s.repo.CreateRelation(ctx, in)
	if err != nil {
		return domain.Relation{}, domain.RelationVersion{}, wrapStoreError(err)
	}
	return rel, v1, nil
}

// CreateVersion appends the next immutable version. The caller must name
// the version it based its change on (expected): if the log has moved past
// that number — because another writer won, or the expectation is stale —
// the call fails with *VersionConflictError and the stable code
// EXPECTED_VERSION_MISMATCH, and the caller re-reads and retries.
func (s *Service) CreateVersion(ctx context.Context, relationID string, expected int, in VersionParams) (domain.RelationVersion, error) {
	if expected < 1 {
		return domain.RelationVersion{}, fmt.Errorf("%w: expected_version must be at least 1 (version 1 is created with the relation)", ErrValidation)
	}
	v, err := normalizeVersionParams(in)
	if err != nil {
		return domain.RelationVersion{}, err
	}
	out, err := s.repo.CreateVersion(ctx, relationID, expected, v)
	if err != nil {
		return domain.RelationVersion{}, wrapStoreError(err)
	}
	return out, nil
}

// GetRelation returns the relation row, or ErrRelationNotFound.
func (s *Service) GetRelation(ctx context.Context, relationID string) (domain.Relation, error) {
	rel, err := s.repo.GetRelation(ctx, relationID)
	if err != nil {
		return domain.Relation{}, wrapStoreError(err)
	}
	return rel, nil
}

// GetVersion returns one version row by number, or
// ErrRelationVersionNotFound.
func (s *Service) GetVersion(ctx context.Context, relationID string, versionNo int) (domain.RelationVersion, error) {
	v, err := s.repo.GetVersion(ctx, relationID, versionNo)
	if err != nil {
		return domain.RelationVersion{}, wrapStoreError(err)
	}
	return v, nil
}

// GetLatestVersion returns the head of the relation's version log, or
// ErrRelationVersionNotFound.
func (s *Service) GetLatestVersion(ctx context.Context, relationID string) (domain.RelationVersion, error) {
	v, err := s.repo.GetLatestVersion(ctx, relationID)
	if err != nil {
		return domain.RelationVersion{}, wrapStoreError(err)
	}
	return v, nil
}

// ListVersions returns the relation's version log, oldest first.
func (s *Service) ListVersions(ctx context.Context, relationID string) ([]domain.RelationVersion, error) {
	vs, err := s.repo.ListVersions(ctx, relationID)
	if err != nil {
		return nil, wrapStoreError(err)
	}
	return vs, nil
}

// ListVersionsByType returns every version of the project with the given
// relation type. Querying an unknown type returns an empty list, not an
// error: no write through the port can store a type outside the catalog,
// so an unknown type has no rows by construction.
func (s *Service) ListVersionsByType(ctx context.Context, projectID string, relationType string) ([]domain.RelationVersion, error) {
	if projectID == "" {
		return nil, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	vs, err := s.repo.ListVersionsByType(ctx, projectID, relationType)
	if err != nil {
		return nil, wrapStoreError(err)
	}
	return vs, nil
}

// ListVersionsByTypes is the category query: every version of the project
// whose type is one of relationTypes. Callers expand a catalog category
// with relationcatalog.DependencyTypes() / ProvenanceTypes() /
// TypesOfCategory().
func (s *Service) ListVersionsByTypes(ctx context.Context, projectID string, relationTypes []string) ([]domain.RelationVersion, error) {
	if projectID == "" {
		return nil, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if len(relationTypes) == 0 {
		return []domain.RelationVersion{}, nil
	}
	vs, err := s.repo.ListVersionsByTypes(ctx, projectID, relationTypes)
	if err != nil {
		return nil, wrapStoreError(err)
	}
	return vs, nil
}

// normalizeVersionParams checks the version content's shape and normalizes
// the payload: empty input becomes the empty metadata object {} (metadata
// is optional for edges, docs/07 §3). The relation type must be in the V1
// catalog — the requirement's catalog validation happens here, before any
// storage. Messages are client-safe fixed strings (docs/22 §5: no
// dependency detail on the wire).
func normalizeVersionParams(in VersionParams) (VersionParams, error) {
	if in.StateID == "" {
		return in, fmt.Errorf("%w: state_id is required", ErrValidation)
	}
	if in.RelationType == "" {
		return in, fmt.Errorf("%w: relation_type is required", ErrValidation)
	}
	if _, ok := relationcatalog.Lookup(in.RelationType); !ok {
		return in, &UnknownRelationTypeError{Type: in.RelationType}
	}
	if in.SourceObjectVersionID == "" {
		return in, fmt.Errorf("%w: source_object_version_id is required (edges are version-pinned)", ErrValidation)
	}
	if in.TargetObjectVersionID == "" {
		return in, fmt.Errorf("%w: target_object_version_id is required (edges are version-pinned)", ErrValidation)
	}
	if in.CreatedBy == "" {
		return in, fmt.Errorf("%w: created_by is required", ErrValidation)
	}
	if len(bytes.TrimSpace(in.Payload)) == 0 {
		in.Payload = json.RawMessage(`{}`)
	}
	var shape any
	if err := json.Unmarshal(in.Payload, &shape); err != nil {
		return in, fmt.Errorf("%w: payload must be valid JSON", ErrValidation)
	}
	if _, ok := shape.(map[string]any); !ok {
		return in, fmt.Errorf("%w: payload must be a JSON object", ErrValidation)
	}
	return in, nil
}

// wrapStoreError keeps the expected domain outcomes (unknown relation or
// version, dangling endpoint, version conflict, unknown type) and turns
// everything else — including an adapter that cannot run (e.g. a migration
// not yet applied) — into ErrStore for the handler, with the cause kept
// for the log.
func wrapStoreError(err error) error {
	if err == nil ||
		errors.Is(err, ErrRelationNotFound) ||
		errors.Is(err, ErrRelationVersionNotFound) ||
		errors.Is(err, ErrReferencedVersionNotFound) ||
		errors.Is(err, ErrValidation) ||
		errors.As(err, new(*VersionConflictError)) ||
		errors.As(err, new(*ReferencedVersionNotFoundError)) ||
		errors.As(err, new(*UnknownRelationTypeError)) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}
