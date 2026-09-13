package sciobjects

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lichman0405/post/internal/domain"
)

// Service orchestrates the scientific object use cases against the
// Repository port. It owns input validation (the port trusts, the service
// verifies) and maps store outcomes onto the package sentinels; it does NOT
// authorize — actors and project membership checks belong to the consuming
// API task, which passes only resolved identities in.
type Service struct {
	repo Repository
}

// NewService wires the service.
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// CreateObject creates the object together with its version 1 — one atomic
// store call, so the pair can never be observed apart.
func (s *Service) CreateObject(ctx context.Context, in CreateObjectParams) (domain.ScientificObject, domain.ScientificObjectVersion, error) {
	if err := validateCreateObject(in); err != nil {
		return domain.ScientificObject{}, domain.ScientificObjectVersion{}, err
	}
	obj, v1, err := s.repo.CreateObject(ctx, in)
	if err != nil {
		return domain.ScientificObject{}, domain.ScientificObjectVersion{}, wrapStoreError(err)
	}
	return obj, v1, nil
}

// CreateVersion appends the next immutable version. The caller must name
// the version it based its change on (expected): if the log has moved past
// that number — because another writer won, or the expectation is stale —
// the call fails with *VersionConflictError and the stable code
// EXPECTED_VERSION_MISMATCH, and the caller re-reads and retries.
func (s *Service) CreateVersion(ctx context.Context, objectID string, expected int, in VersionParams) (domain.ScientificObjectVersion, error) {
	if err := validateVersionParams(in); err != nil {
		return domain.ScientificObjectVersion{}, err
	}
	if expected < 1 {
		return domain.ScientificObjectVersion{}, fmt.Errorf("%w: expected_version must be at least 1 (version 1 is created with the object)", ErrValidation)
	}
	v, err := s.repo.CreateVersion(ctx, objectID, expected, in)
	if err != nil {
		return domain.ScientificObjectVersion{}, wrapStoreError(err)
	}
	return v, nil
}

// GetObject returns the object row, or ErrObjectNotFound.
func (s *Service) GetObject(ctx context.Context, objectID string) (domain.ScientificObject, error) {
	obj, err := s.repo.GetObject(ctx, objectID)
	if err != nil {
		return domain.ScientificObject{}, wrapStoreError(err)
	}
	return obj, nil
}

// GetVersion returns one version row by number, or ErrVersionNotFound.
func (s *Service) GetVersion(ctx context.Context, objectID string, versionNo int) (domain.ScientificObjectVersion, error) {
	v, err := s.repo.GetVersion(ctx, objectID, versionNo)
	if err != nil {
		return domain.ScientificObjectVersion{}, wrapStoreError(err)
	}
	return v, nil
}

// GetLatestVersion returns the head of the object's version log, or
// ErrVersionNotFound.
func (s *Service) GetLatestVersion(ctx context.Context, objectID string) (domain.ScientificObjectVersion, error) {
	v, err := s.repo.GetLatestVersion(ctx, objectID)
	if err != nil {
		return domain.ScientificObjectVersion{}, wrapStoreError(err)
	}
	return v, nil
}

// ListVersions returns the object's version log, oldest first.
func (s *Service) ListVersions(ctx context.Context, objectID string) ([]domain.ScientificObjectVersion, error) {
	vs, err := s.repo.ListVersions(ctx, objectID)
	if err != nil {
		return nil, wrapStoreError(err)
	}
	return vs, nil
}

// validateCreateObject checks the identity facts; the version content is
// checked by validateVersionParams.
func validateCreateObject(in CreateObjectParams) error {
	if in.ProjectID == "" {
		return fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if in.ObjectType == "" {
		return fmt.Errorf("%w: object_type is required", ErrValidation)
	}
	if in.CreatedBy == "" {
		return fmt.Errorf("%w: created_by is required", ErrValidation)
	}
	return validateVersionParams(in.Version)
}

// validateVersionParams checks the version content's shape. Payload must be
// a non-empty JSON object: every scientific object inherits the
// CoreScientificObject shape (docs/08), which is an object — per-type
// schema validation against the registry (internal/rsg/schemareg, T0201)
// is wired by the consuming API task. Messages are client-safe fixed
// strings (docs/22 §5: no dependency detail on the wire).
func validateVersionParams(in VersionParams) error {
	if in.StateID == "" {
		return fmt.Errorf("%w: state_id is required", ErrValidation)
	}
	if in.SchemaID == "" || in.SchemaVersion == "" {
		return fmt.Errorf("%w: schema_ref (id and version) is required", ErrValidation)
	}
	if in.Title == "" {
		return fmt.Errorf("%w: title is required", ErrValidation)
	}
	if !domain.ValidLifecycleState(string(in.LifecycleState)) {
		return fmt.Errorf("%w: lifecycle_state must be one of active, aborted, reopened, superseded", ErrValidation)
	}
	if in.CreatedBy == "" {
		return fmt.Errorf("%w: created_by is required", ErrValidation)
	}
	if len(bytes.TrimSpace(in.Payload)) == 0 {
		return fmt.Errorf("%w: payload is required", ErrValidation)
	}
	var shape any
	if err := json.Unmarshal(in.Payload, &shape); err != nil {
		return fmt.Errorf("%w: payload must be valid JSON", ErrValidation)
	}
	if _, ok := shape.(map[string]any); !ok {
		return fmt.Errorf("%w: payload must be a JSON object", ErrValidation)
	}
	return nil
}

// wrapStoreError keeps the expected domain outcomes (unknown object or
// version, version conflict) and turns everything else — including an
// adapter that cannot run (e.g. a migration not yet applied) — into
// ErrStore for the handler, with the cause kept for the log.
func wrapStoreError(err error) error {
	if err == nil ||
		errors.Is(err, ErrObjectNotFound) ||
		errors.Is(err, ErrVersionNotFound) ||
		errors.As(err, new(*VersionConflictError)) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}
