package persistence

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// The settings half of the project store (T0109): the member list, the
// role change and the purpose/activity-status edit. Both writes record
// their audit entry in the SAME transaction as the state change via the
// shared appendAudit helper (docs/53: audit + state are one unit — a
// governance action can never land without its audit row, and vice
// versa). The entries are T0110's domain.AuditEntry: settings actions and
// the audit application surface are two halves of the same audit_log
// table, so there is one type, one writer and one action-name registry;
// T0110's Activity queries read the rows written here.

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
		return appendAudit(ctx, q, audit)
	})
	if err != nil {
		return domain.ProjectMembership{}, err
	}
	return updated, nil
}

// AddProjectMembership implements projects.ProjectStore (T1335): inserts
// one membership for an account the service already resolved from its
// handle, and records the audit entry in the SAME transaction. The project
// row is locked first — the same project-wide serialization point
// UpdateMembershipRole takes — so a concurrent role change, remove or
// second invite can never interleave with an add. A duplicate membership
// arrives as the primary-key conflict (23505 on project_memberships) and
// is answered as ErrAlreadyMember: two racing invites add exactly one row,
// and the loser learns the person is already a member instead of the store
// failing.
func (s *ProjectStore) AddProjectMembership(ctx context.Context, projectID, userID string, role domain.ProjectRole, audit domain.AuditEntry) (domain.ProjectMembership, error) {
	pID, uID, err := twoUUIDs(projectID, userID)
	if err != nil {
		// The service resolves the user from the users table, so a
		// malformed id here is a malformed project id: answer the same
		// not-found an unknown project answers.
		return domain.ProjectMembership{}, projects.ErrProjectNotFound
	}
	var added domain.ProjectMembership
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if _, err := q.GetProjectByIDForUpdate(ctx, pID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return projects.ErrProjectNotFound
			}
			return err
		}
		row, err := q.AddProjectMembership(ctx, sqlc.AddProjectMembershipParams{
			ProjectID: pID,
			UserID:    uID,
			Role:      string(role),
		})
		if err != nil {
			return mapProjectWriteError(err)
		}
		added = projectMembershipFromRow(row)
		return appendAudit(ctx, q, audit)
	})
	if err != nil {
		return domain.ProjectMembership{}, err
	}
	return added, nil
}

// GetUserByHandle implements projects.ProjectStore (T1335): the invite
// path names a person by handle, so the adapter resolves the account
// before the membership insert. An unknown handle is ErrUserNotFound —
// V1 has no pending invitations, and the service turns that into the
// wire's USER_NOT_FOUND rather than inventing an account.
func (s *ProjectStore) GetUserByHandle(ctx context.Context, handle string) (domain.User, error) {
	row, err := sqlc.New(s.pool).GetUserByHandle(ctx, handle)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.User{}, projects.ErrUserNotFound
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("persistence: get user by handle: %w", err)
	}
	return domain.User{
		ID:          pgUUIDToText(row.ID),
		Handle:      row.Handle,
		Email:       nullStringToValue(row.Email),
		DisplayName: row.DisplayName,
		CreatedAt:   row.CreatedAt.Time,
	}, nil
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
		return appendAudit(ctx, q, audit)
	})
	if err != nil {
		return domain.Project{}, err
	}
	return updated, nil
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
