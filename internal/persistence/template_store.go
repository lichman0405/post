package persistence

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/templates"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// TemplateStore is the production templates.InstantiationStore adapter
// over PostgreSQL (sqlc generated queries, pgx). The table is append-only
// (the 00056 triggers reject UPDATE/DELETE at the database): the
// provenance fact "project X was created from template Y version Z" never
// changes and never disappears.
type TemplateStore struct {
	pool *pgxpool.Pool
}

// NewTemplateStore builds the store on pool.
func NewTemplateStore(pool *pgxpool.Pool) *TemplateStore {
	return &TemplateStore{pool: pool}
}

// RecordInstantiation implements templates.InstantiationStore: inserts
// the provenance row. A second record for the same project answers
// templates.ErrInstantiationExists (23505 on UNIQUE (project_id) — one
// project has at most one template origin). Everything else is returned
// raw, including a project_id/created_by foreign-key violation (23503):
// the service turns it into templates.ErrStore, which lands in the applied
// report where it is visible — never silently swallowed, and no invented
// wire code for a row the caller cannot have produced.
func (s *TemplateStore) RecordInstantiation(ctx context.Context, r domain.TemplateInstantiation) (domain.TemplateInstantiation, error) {
	projectID, err := textUUID(r.ProjectID)
	if err != nil {
		return domain.TemplateInstantiation{}, templates.ErrValidation
	}
	createdBy, err := textUUID(r.CreatedBy)
	if err != nil {
		return domain.TemplateInstantiation{}, templates.ErrValidation
	}
	row, err := sqlc.New(s.pool).InsertTemplateInstantiation(ctx, sqlc.InsertTemplateInstantiationParams{
		ProjectID:       projectID,
		TemplateID:      r.TemplateID,
		TemplateVersion: r.TemplateVersion,
		TemplateName:    r.TemplateName,
		CreatedBy:       createdBy,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return domain.TemplateInstantiation{}, templates.ErrInstantiationExists
		}
		return domain.TemplateInstantiation{}, err
	}
	return templateInstantiationFromRow(row), nil
}

// GetInstantiation implements templates.InstantiationStore: the project's
// one template origin, or templates.ErrInstantiationNotFound when the
// project was created without a template.
func (s *TemplateStore) GetInstantiation(ctx context.Context, projectID string) (domain.TemplateInstantiation, error) {
	id, err := textUUID(projectID)
	if err != nil {
		return domain.TemplateInstantiation{}, templates.ErrInstantiationNotFound
	}
	row, err := sqlc.New(s.pool).GetTemplateInstantiationByProject(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.TemplateInstantiation{}, templates.ErrInstantiationNotFound
		}
		return domain.TemplateInstantiation{}, err
	}
	return templateInstantiationFromRow(row), nil
}

// templateInstantiationFromRow renders a sqlc row into the domain shape.
func templateInstantiationFromRow(row sqlc.ProjectTemplateInstantiation) domain.TemplateInstantiation {
	return domain.TemplateInstantiation{
		ID:              pgUUIDToText(row.ID),
		ProjectID:       pgUUIDToText(row.ProjectID),
		TemplateID:      row.TemplateID,
		TemplateVersion: row.TemplateVersion,
		TemplateName:    row.TemplateName,
		CreatedBy:       pgUUIDToText(row.CreatedBy),
		CreatedAt:       row.CreatedAt.Time,
	}
}
