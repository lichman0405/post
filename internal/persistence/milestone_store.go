package persistence

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/milestones"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// MilestoneStore is the production project-milestones adapter over
// PostgreSQL (queries/milestones.sql, migration 00063): the timeline
// write/read surface — the create transaction (milestone row +
// idempotency ledger entry + audit row) and the timeline reads
// (occurred_at ascending, creation order breaking date ties).
type MilestoneStore struct {
	pool *pgxpool.Pool
}

// NewMilestoneStore builds the store on pool. The pool may be lazy
// (OpenLazy): the API keeps starting while PostgreSQL is down.
func NewMilestoneStore(pool *pgxpool.Pool) *MilestoneStore {
	return &MilestoneStore{pool: pool}
}

// CreateMilestone implements milestones.StorePort. The insert runs in
// one transaction that row-locks the project (GetProjectByIDForUpdate —
// the same lock membership writes take, so concurrent milestone creates
// for the project serialize) and commits the milestone row, its
// idempotency ledger entry and its audit row together.
//
// A known Idempotency-Key replays instead of inserting: under the
// project lock the ledger read is race-free, and the replay returns the
// first create's row without writing anything (a replay is a read).
func (s *MilestoneStore) CreateMilestone(ctx context.Context, m domain.Milestone, audit domain.AuditEntry, idempotencyKey *string) (domain.Milestone, error) {
	projectID, err := textUUID(m.ProjectID)
	if err != nil {
		return domain.Milestone{}, milestones.ErrProjectNotFound
	}
	createdBy, err := textUUID(m.CreatedBy)
	if err != nil {
		return domain.Milestone{}, fmt.Errorf("persistence: milestone creator id: %w", err)
	}
	releaseID, err := optionalUUIDPtr(m.ReleaseID)
	if err != nil {
		return domain.Milestone{}, fmt.Errorf("persistence: milestone release link: %w", err)
	}
	var label *string
	if m.Label != "" {
		l := m.Label
		label = &l
	}
	var created domain.Milestone
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if _, err := q.GetProjectByIDForUpdate(ctx, projectID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return milestones.ErrProjectNotFound
			}
			return err
		}
		if idempotencyKey != nil {
			milestoneID, err := q.GetMilestoneCreation(ctx, sqlc.GetMilestoneCreationParams{
				ProjectID:      projectID,
				IdempotencyKey: *idempotencyKey,
			})
			switch {
			case err == nil:
				row, err := q.GetMilestone(ctx, sqlc.GetMilestoneParams{ProjectID: projectID, ID: milestoneID})
				if err != nil {
					return err
				}
				created, err = milestoneFromRow(row)
				return err
			case errors.Is(err, pgx.ErrNoRows):
				// first create with this key — fall through to the insert
			default:
				return err
			}
		}
		row, err := q.CreateMilestone(ctx, sqlc.CreateMilestoneParams{
			ProjectID:  projectID,
			Kind:       string(m.Kind),
			Label:      label,
			OccurredAt: pgTimestamptz(m.OccurredAt),
			ReleaseID:  releaseID,
			CreatedBy:  createdBy,
		})
		if err != nil {
			return err
		}
		created, err = milestoneFromRow(row)
		if err != nil {
			return err
		}
		if idempotencyKey != nil {
			milestoneID, err := textUUID(created.ID)
			if err != nil {
				return err
			}
			if _, err := q.CreateMilestoneCreation(ctx, sqlc.CreateMilestoneCreationParams{
				ProjectID:      projectID,
				IdempotencyKey: *idempotencyKey,
				MilestoneID:    milestoneID,
			}); err != nil {
				return err
			}
		}
		// The audit row names the assigned milestone id — the store
		// writes it after the insert, so the command left the target ref
		// empty.
		audit.TargetRef = "milestone:" + created.ID
		return appendAudit(ctx, q, audit)
	})
	if err != nil {
		return domain.Milestone{}, mapMilestoneWriteError(err)
	}
	return created, nil
}

// LookupCreation implements the command's replay fast path: the milestone
// an Idempotency-Key already created, or nil when the key has no entry
// yet (the command checks it before resolving the release link, so a
// replay never re-validates anything — the same key returns the first
// create's row forever).
func (s *MilestoneStore) LookupCreation(ctx context.Context, projectID, idempotencyKey string) (*domain.Milestone, error) {
	pID, err := textUUID(projectID)
	if err != nil {
		return nil, milestones.ErrProjectNotFound
	}
	q := sqlc.New(s.pool)
	milestoneID, err := q.GetMilestoneCreation(ctx, sqlc.GetMilestoneCreationParams{
		ProjectID:      pID,
		IdempotencyKey: idempotencyKey,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("persistence: read milestone creation: %w", err)
	}
	row, err := q.GetMilestone(ctx, sqlc.GetMilestoneParams{ProjectID: pID, ID: milestoneID})
	if err != nil {
		return nil, fmt.Errorf("persistence: read milestone: %w", err)
	}
	m, err := milestoneFromRow(row)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// ListMilestones implements milestones.StorePort — the project's
// timeline: occurred_at ascending (the events' dates, the canonical
// kinds' natural progression), creation order (created_at, id) breaking
// date ties. The ORDER BY is the query's (queries/milestones.sql).
func (s *MilestoneStore) ListMilestones(ctx context.Context, projectID string) ([]domain.Milestone, error) {
	pID, err := textUUID(projectID)
	if err != nil {
		return nil, milestones.ErrProjectNotFound
	}
	rows, err := sqlc.New(s.pool).ListMilestones(ctx, pID)
	if err != nil {
		return nil, fmt.Errorf("persistence: list milestones: %w", err)
	}
	out := make([]domain.Milestone, 0, len(rows))
	for _, row := range rows {
		m, err := milestoneFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// GetMilestone implements milestones.StorePort: one milestone of the
// project; a milestone of another project — or an unknown id — answers
// milestones.ErrMilestoneNotFound (existence hiding, docs/45).
func (s *MilestoneStore) GetMilestone(ctx context.Context, projectID, milestoneID string) (domain.Milestone, error) {
	pID, err := textUUID(projectID)
	if err != nil {
		return domain.Milestone{}, milestones.ErrProjectNotFound
	}
	mID, err := textUUID(milestoneID)
	if err != nil {
		return domain.Milestone{}, milestones.ErrMilestoneNotFound
	}
	row, err := sqlc.New(s.pool).GetMilestone(ctx, sqlc.GetMilestoneParams{ProjectID: pID, ID: mID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Milestone{}, milestones.ErrMilestoneNotFound
		}
		return domain.Milestone{}, fmt.Errorf("persistence: read milestone: %w", err)
	}
	return milestoneFromRow(row)
}

// milestoneFromRow converts a sqlc project_milestones row to the domain
// value. The label is a nullable text column: NULL renders as the empty
// string (no custom label — the kind's display name renders then).
func milestoneFromRow(row sqlc.ProjectMilestone) (domain.Milestone, error) {
	if !row.ID.Valid {
		return domain.Milestone{}, fmt.Errorf("persistence: milestone row without id")
	}
	label := ""
	if row.Label != nil {
		label = *row.Label
	}
	return domain.Milestone{
		ID:         pgUUIDToText(row.ID),
		ProjectID:  pgUUIDToText(row.ProjectID),
		Kind:       domain.MilestoneKind(row.Kind),
		Label:      label,
		OccurredAt: row.OccurredAt.Time,
		ReleaseID:  uuidPtr(row.ReleaseID),
		CreatedBy:  pgUUIDToText(row.CreatedBy),
		CreatedAt:  row.CreatedAt.Time,
	}, nil
}

// pgTimestamptz renders a domain timestamp as the pgtype value the sqlc
// queries expect.
func pgTimestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// mapMilestoneWriteError keeps the store's own sentinels (the command's
// contract) and wraps everything else with the persistence context.
func mapMilestoneWriteError(err error) error {
	if err == nil || errors.Is(err, milestones.ErrProjectNotFound) {
		return err
	}
	return fmt.Errorf("persistence: milestone write: %w", err)
}
