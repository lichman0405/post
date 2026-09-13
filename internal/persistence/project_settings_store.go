package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// The settings half of the project store (T0109): the member list, the
// role change and the purpose/activity-status edit. Both writes record
// their audit placeholder in the SAME transaction as the state change
// (docs/53: audit + state are one unit — a governance action can never
// land without its audit row, and vice versa). T0110 builds the audit
// application surface on these rows.

// ListProjectMembers implements projects.ProjectStore.
func (s *ProjectStore) ListProjectMembers(ctx context.Context, projectID string) ([]domain.ProjectMember, error) {
	id, err := textUUID(projectID)
	if err != nil {
		return nil, projects.ErrProjectNotFound
	}
	rows, err := sqlc.New(s.pool).ListProjectMembers(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("persistence: list project members: %w", err)
	}
	out := make([]domain.ProjectMember, 0, len(rows))
	for _, row := range rows {
		out = append(out, projectMemberFromRow(row))
	}
	return out, nil
}

// UpdateMembershipRole implements projects.ProjectStore. The write runs
// in one transaction that first locks the project row — the project-wide
// serialization point for membership changes — then re-reads the target
// membership (FOR UPDATE), refuses the demotion of the last owner under
// that lock, updates the role and records the audit entry. The lock
// closes the two-concurrent-demotions race: with the project row held,
// the owner count below cannot be stale.
func (s *ProjectStore) UpdateMembershipRole(ctx context.Context, projectID, userID string, role domain.ProjectRole, audit domain.AuditEntry) (domain.ProjectMembership, error) {
	pID, uID, err := twoUUIDs(projectID, userID)
	if err != nil {
		return domain.ProjectMembership{}, projects.ErrTargetMemberNotFound
	}
	var updated domain.ProjectMembership
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if _, err := q.GetProjectByIDForUpdate(ctx, pID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return projects.ErrProjectNotFound
			}
			return err
		}
		current, err := q.GetProjectMembership(ctx, sqlc.GetProjectMembershipParams{
			ProjectID: pID,
			UserID:    uID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return projects.ErrTargetMemberNotFound
		}
		if err != nil {
			return err
		}
		if current.Role == string(domain.ProjectRoleOwner) && role != domain.ProjectRoleOwner {
			owners, err := q.CountProjectOwners(ctx, pID)
			if err != nil {
				return err
			}
			if owners <= 1 {
				return projects.ErrLastOwner
			}
		}
		row, err := q.UpdateProjectMembershipRole(ctx, sqlc.UpdateProjectMembershipRoleParams{
			ProjectID: pID,
			UserID:    uID,
			Role:      string(role),
		})
		if err != nil {
			return mapProjectWriteError(err)
		}
		updated = projectMembershipFromRow(row)
		return insertAuditEntry(ctx, q, audit)
	})
	if err != nil {
		return domain.ProjectMembership{}, err
	}
	return updated, nil
}

// UpdateProjectSettings implements projects.ProjectStore: applies the
// edit (only the non-nil fields) and records the audit entry in the same
// transaction.
func (s *ProjectStore) UpdateProjectSettings(ctx context.Context, projectID string, purpose, activityStatus *string, audit domain.AuditEntry) (domain.Project, error) {
	id, err := textUUID(projectID)
	if err != nil {
		return domain.Project{}, projects.ErrProjectNotFound
	}
	var updated domain.Project
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		current, err := q.GetProjectByIDForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return projects.ErrProjectNotFound
		}
		if err != nil {
			return err
		}
		newPurpose := current.Purpose
		if purpose != nil {
			newPurpose = *purpose
		}
		newStatus := current.ActivityStatus
		if activityStatus != nil {
			newStatus = *activityStatus
		}
		row, err := q.UpdateProjectSettings(ctx, sqlc.UpdateProjectSettingsParams{
			ID:             id,
			Purpose:        newPurpose,
			ActivityStatus: newStatus,
		})
		if err != nil {
			return mapProjectWriteError(err)
		}
		updated = projectFromRow(row)
		return insertAuditEntry(ctx, q, audit)
	})
	if err != nil {
		return domain.Project{}, err
	}
	return updated, nil
}

// insertAuditEntry writes one audit_log row (the T0109 placeholder;
// T0110 owns the audit store). The entry's summaries are marshalled to
// JSONB here; a malformed actor/project id fails the transaction —
// fail closed, a governance write without its audit row must not land.
func insertAuditEntry(ctx context.Context, q *sqlc.Queries, e domain.AuditEntry) error {
	actorID, err := textUUID(e.ActorID)
	if err != nil {
		return fmt.Errorf("persistence: audit actor id: %w", err)
	}
	projectID, err := textUUID(e.ProjectID)
	if err != nil {
		return fmt.Errorf("persistence: audit project id: %w", err)
	}
	before, err := json.Marshal(e.Before)
	if err != nil {
		return fmt.Errorf("persistence: audit before summary: %w", err)
	}
	after, err := json.Marshal(e.After)
	if err != nil {
		return fmt.Errorf("persistence: audit after summary: %w", err)
	}
	_, err = q.RecordAuditLogEntry(ctx, sqlc.RecordAuditLogEntryParams{
		ActorID:       actorID,
		Via:           e.Via,
		Action:        e.Action,
		TargetRef:     e.TargetRef,
		ProjectID:     projectID,
		CorrelationID: e.CorrelationID,
		BeforeSummary: before,
		AfterSummary:  after,
		Metadata:      []byte(`{}`),
	})
	if err != nil {
		return fmt.Errorf("persistence: record audit entry: %w", err)
	}
	return nil
}

// projectMemberFromRow converts a sqlc member-list row to the domain
// value.
func projectMemberFromRow(row sqlc.ListProjectMembersRow) domain.ProjectMember {
	return domain.ProjectMember{
		UserID:      pgUUIDToText(row.UserID),
		Handle:      row.Handle,
		DisplayName: row.DisplayName,
		Role:        domain.ProjectRole(row.Role),
		JoinedAt:    row.JoinedAt.Time,
	}
}
