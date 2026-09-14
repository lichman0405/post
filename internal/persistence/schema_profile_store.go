package persistence

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/schemaprofiles"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// SchemaProfileStore is the production schemaprofiles.ProfileStore adapter
// over PostgreSQL (sqlc generated queries, pgx). The table is append-only
// (the 00038 trigger rejects UPDATE/DELETE at the database): every profile
// change is a new version row, so old object versions keep their pinned
// schema content forever (docs/21 §8).
type SchemaProfileStore struct {
	pool *pgxpool.Pool
}

// NewSchemaProfileStore builds the store on pool.
func NewSchemaProfileStore(pool *pgxpool.Pool) *SchemaProfileStore {
	return &SchemaProfileStore{pool: pool}
}

// RegisterProfile implements schemaprofiles.ProfileStore: inserts one
// immutable profile version and its audit entry in the SAME transaction
// (docs/53: the audit record of a governance action is part of the action,
// never a separate best-effort write). A duplicate (project, schema_id,
// version) answers ErrProfileVersionExists — content is never overwritten.
func (s *SchemaProfileStore) RegisterProfile(ctx context.Context, p domain.ProjectSchemaProfile, audit domain.AuditEntry) (domain.ProjectSchemaProfile, error) {
	projectID, err := textUUID(p.ProjectID)
	if err != nil {
		return domain.ProjectSchemaProfile{}, schemaprofiles.ErrProjectNotFound
	}
	createdBy, err := textUUID(p.CreatedBy)
	if err != nil {
		return domain.ProjectSchemaProfile{}, fmt.Errorf("persistence: schema profile creator id: %w", err)
	}
	var stored domain.ProjectSchemaProfile
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		row, err := q.InsertSchemaProfile(ctx, sqlc.InsertSchemaProfileParams{
			ProjectID:         projectID,
			SchemaID:          p.SchemaID,
			Version:           p.Version,
			BaseSchemaID:      p.BaseSchemaID,
			BaseSchemaVersion: p.BaseSchemaVersion,
			Content:           p.Content,
			ContentHash:       p.ContentHash,
			CreatedBy:         createdBy,
		})
		if err != nil {
			return mapSchemaProfileWriteError(err)
		}
		stored = schemaProfileFromRow(row)
		return appendAudit(ctx, q, audit)
	})
	if err != nil {
		return domain.ProjectSchemaProfile{}, err
	}
	return stored, nil
}

// GetProfile implements schemaprofiles.ProfileStore: one version by
// (project, schema_id, version) — any age.
func (s *SchemaProfileStore) GetProfile(ctx context.Context, projectID, schemaID, version string) (domain.ProjectSchemaProfile, error) {
	projectUUID, err := textUUID(projectID)
	if err != nil {
		return domain.ProjectSchemaProfile{}, schemaprofiles.ErrProfileNotFound
	}
	row, err := sqlc.New(s.pool).GetSchemaProfile(ctx, sqlc.GetSchemaProfileParams{
		ProjectID: projectUUID,
		SchemaID:  schemaID,
		Version:   version,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ProjectSchemaProfile{}, schemaprofiles.ErrProfileNotFound
	}
	if err != nil {
		return domain.ProjectSchemaProfile{}, fmt.Errorf("persistence: get schema profile: %w", err)
	}
	return schemaProfileFromRow(row), nil
}

// GetLatestProfile implements schemaprofiles.ProfileStore: the profile's
// newest registered version (created_at DESC, id DESC tie-break).
func (s *SchemaProfileStore) GetLatestProfile(ctx context.Context, projectID, schemaID string) (domain.ProjectSchemaProfile, error) {
	projectUUID, err := textUUID(projectID)
	if err != nil {
		return domain.ProjectSchemaProfile{}, schemaprofiles.ErrProfileNotFound
	}
	row, err := sqlc.New(s.pool).GetLatestSchemaProfile(ctx, sqlc.GetLatestSchemaProfileParams{
		ProjectID: projectUUID,
		SchemaID:  schemaID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ProjectSchemaProfile{}, schemaprofiles.ErrProfileNotFound
	}
	if err != nil {
		return domain.ProjectSchemaProfile{}, fmt.Errorf("persistence: get latest schema profile: %w", err)
	}
	return schemaProfileFromRow(row), nil
}

// ListProfiles implements schemaprofiles.ProfileStore: every profile
// version of the project, newest first.
func (s *SchemaProfileStore) ListProfiles(ctx context.Context, projectID string) ([]domain.ProjectSchemaProfile, error) {
	projectUUID, err := textUUID(projectID)
	if err != nil {
		return nil, schemaprofiles.ErrProjectNotFound
	}
	rows, err := sqlc.New(s.pool).ListProjectSchemaProfiles(ctx, projectUUID)
	if err != nil {
		return nil, fmt.Errorf("persistence: list schema profiles: %w", err)
	}
	out := make([]domain.ProjectSchemaProfile, 0, len(rows))
	for _, row := range rows {
		out = append(out, schemaProfileFromRow(row))
	}
	return out, nil
}

// ListAllProfiles implements schemaprofiles.ProfileStore: every registered
// profile row of every project — the API startup load re-registers these
// into the in-memory schema registry.
func (s *SchemaProfileStore) ListAllProfiles(ctx context.Context) ([]domain.ProjectSchemaProfile, error) {
	rows, err := sqlc.New(s.pool).ListAllSchemaProfiles(ctx)
	if err != nil {
		return nil, fmt.Errorf("persistence: list all schema profiles: %w", err)
	}
	out := make([]domain.ProjectSchemaProfile, 0, len(rows))
	for _, row := range rows {
		out = append(out, schemaProfileFromRow(row))
	}
	return out, nil
}

// schemaProfileFromRow converts a sqlc profile row to the domain value.
// The generated row type is shared by every :one/:many query above (all
// SELECT *), so one converter serves them all.
func schemaProfileFromRow(row sqlc.ProjectSchemaProfile) domain.ProjectSchemaProfile {
	return domain.ProjectSchemaProfile{
		ID:                pgUUIDToText(row.ID),
		ProjectID:         pgUUIDToText(row.ProjectID),
		SchemaID:          row.SchemaID,
		Version:           row.Version,
		BaseSchemaID:      row.BaseSchemaID,
		BaseSchemaVersion: row.BaseSchemaVersion,
		Content:           row.Content,
		ContentHash:       row.ContentHash,
		CreatedBy:         pgUUIDToText(row.CreatedBy),
		CreatedAt:         row.CreatedAt.Time,
	}
}

// mapSchemaProfileWriteError translates project_schema_profiles driver
// errors onto the schemaprofiles service's sentinels.
func mapSchemaProfileWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "23505" && strings.Contains(pgErr.ConstraintName, "project_schema_profiles_project_id_schema_id_version"):
			// The unique index name: project_schema_profiles_project_id_schema_id_version_key.
			return schemaprofiles.ErrProfileVersionExists
		case pgErr.Code == "23503" && strings.Contains(pgErr.ConstraintName, "project_id"):
			return schemaprofiles.ErrProjectNotFound
		case pgErr.Code == "23514" && strings.Contains(pgErr.ConstraintName, "project_schema_profiles"):
			return schemaprofiles.ErrValidation
		}
	}
	return fmt.Errorf("persistence: register schema profile: %w", err)
}

var _ schemaprofiles.ProfileStore = (*SchemaProfileStore)(nil)
