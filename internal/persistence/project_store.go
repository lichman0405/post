package persistence

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// ProjectStore is the production projects.ProjectStore adapter over
// PostgreSQL (sqlc generated queries, pgx). The creation invariants that
// must hold under concurrency (organization active + active membership +
// program belongs to the organization) run inside one transaction per
// create, serialized per organization by a FOR UPDATE lock on the
// organization row.
type ProjectStore struct {
	pool *pgxpool.Pool
}

// NewProjectStore builds the store on pool. The pool may be lazy
// (OpenLazy): the API keeps starting while PostgreSQL is down.
func NewProjectStore(pool *pgxpool.Pool) *ProjectStore {
	return &ProjectStore{pool: pool}
}

// CreateProject implements projects.ProjectStore: inserts the project and
// the creator's owner membership in one transaction — a project can never
// exist without its first owner. The project row is created
// provision-pending (the canonical column default; T0301 provisions the
// GitProvider repository later).
//
// Organization projects: the organization row is locked and re-checked
// (exists, not deactivated) and the creator's membership is re-checked
// (exists, active) — a create racing a concurrent deactivation or
// affiliation end can never land a project in a deactivated organization
// or for a departed member. The program reference, when present, must
// belong to the same organization.
func (s *ProjectStore) CreateProject(ctx context.Context, p domain.Project, creatorUserID string) (domain.Project, domain.ProjectMembership, error) {
	creatorID, err := textUUID(creatorUserID)
	if err != nil {
		return domain.Project{}, domain.ProjectMembership{}, fmt.Errorf("persistence: creator id: %w", err)
	}
	var orgID, programID pgtype.UUID
	if p.OrganizationID != nil {
		orgID, err = textUUID(*p.OrganizationID)
		if err != nil {
			return domain.Project{}, domain.ProjectMembership{}, projects.ErrOrgNotFound
		}
	}
	if p.ProgramID != nil {
		programID, err = textUUID(*p.ProgramID)
		if err != nil {
			return domain.Project{}, domain.ProjectMembership{}, projects.ErrProgramNotFound
		}
	}
	var created domain.Project
	var membership domain.ProjectMembership
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if orgID.Valid {
			orgRow, err := q.GetOrganizationByIDForUpdate(ctx, orgID)
			if errors.Is(err, pgx.ErrNoRows) {
				return projects.ErrOrgNotFound
			}
			if err != nil {
				return err
			}
			if !orgFromRow(orgRow).Active() {
				return projects.ErrOrgDeactivated
			}
			mRow, err := q.GetOrganizationMembership(ctx, sqlc.GetOrganizationMembershipParams{
				OrganizationID: orgID,
				UserID:         creatorID,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return projects.ErrForbidden
			}
			if err != nil {
				return err
			}
			if !membershipFromRow(mRow).Active() {
				return projects.ErrForbidden
			}
		}
		if programID.Valid {
			progRow, err := q.GetProgramByID(ctx, programID)
			if errors.Is(err, pgx.ErrNoRows) {
				return projects.ErrProgramNotFound
			}
			if err != nil {
				return err
			}
			sameOrg := progRow.OrganizationID.Valid && orgID.Valid &&
				pgUUIDToText(progRow.OrganizationID) == pgUUIDToText(orgID)
			if !sameOrg {
				return projects.ErrProgramOrgMismatch
			}
		}
		row, err := q.CreateProject(ctx, sqlc.CreateProjectParams{
			OrganizationID: orgID,
			ProgramID:      programID,
			Slug:           p.Slug,
			Name:           p.Name,
			Purpose:        p.Purpose,
			Visibility:     string(p.Visibility),
			CreatedBy:      creatorID,
		})
		if err != nil {
			return mapProjectWriteError(err)
		}
		created = projectFromRow(row)
		mRow, err := q.AddProjectMembership(ctx, sqlc.AddProjectMembershipParams{
			ProjectID: row.ID,
			UserID:    creatorID,
			Role:      string(domain.ProjectRoleOwner),
		})
		if err != nil {
			return mapProjectWriteError(err)
		}
		membership = projectMembershipFromRow(mRow)
		return nil
	})
	if err != nil {
		return domain.Project{}, domain.ProjectMembership{}, err
	}
	return created, membership, nil
}

// GetProject implements projects.ProjectStore.
func (s *ProjectStore) GetProject(ctx context.Context, projectID string) (domain.Project, error) {
	id, err := textUUID(projectID)
	if err != nil {
		return domain.Project{}, projects.ErrProjectNotFound
	}
	row, err := sqlc.New(s.pool).GetProjectByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Project{}, projects.ErrProjectNotFound
	}
	if err != nil {
		return domain.Project{}, fmt.Errorf("persistence: get project: %w", err)
	}
	return projectFromRow(row), nil
}

// GetMembership implements projects.ProjectStore.
func (s *ProjectStore) GetMembership(ctx context.Context, projectID, userID string) (domain.ProjectMembership, error) {
	pID, uID, err := twoUUIDs(projectID, userID)
	if err != nil {
		return domain.ProjectMembership{}, projects.ErrMemberNotFound
	}
	row, err := sqlc.New(s.pool).GetProjectMembership(ctx, sqlc.GetProjectMembershipParams{
		ProjectID: pID,
		UserID:    uID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ProjectMembership{}, projects.ErrMemberNotFound
	}
	if err != nil {
		return domain.ProjectMembership{}, fmt.Errorf("persistence: get project membership: %w", err)
	}
	return projectMembershipFromRow(row), nil
}

// ListProjectsForUser implements projects.ProjectStore.
func (s *ProjectStore) ListProjectsForUser(ctx context.Context, userID string) ([]domain.Project, error) {
	id, err := textUUID(userID)
	if err != nil {
		return nil, nil // an id that is not a uuid matches no memberships
	}
	rows, err := sqlc.New(s.pool).ListProjectsForUser(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("persistence: list projects: %w", err)
	}
	out := make([]domain.Project, 0, len(rows))
	for _, row := range rows {
		out = append(out, projectFromRow(row))
	}
	return out, nil
}

// GetProgram implements projects.ProjectStore.
func (s *ProjectStore) GetProgram(ctx context.Context, programID string) (domain.Program, error) {
	id, err := textUUID(programID)
	if err != nil {
		return domain.Program{}, projects.ErrProgramNotFound
	}
	row, err := sqlc.New(s.pool).GetProgramByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Program{}, projects.ErrProgramNotFound
	}
	if err != nil {
		return domain.Program{}, fmt.Errorf("persistence: get program: %w", err)
	}
	return programFromRow(row), nil
}

// mapProjectWriteError translates project-table driver errors. The slug
// conflicts carry constraint names containing "slug" (both the
// organization-scoped UNIQUE constraint and the personal-project partial
// unique index), so one branch covers both.
func mapProjectWriteError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, projects.ErrOrgNotFound),
		errors.Is(err, projects.ErrOrgDeactivated),
		errors.Is(err, projects.ErrForbidden),
		errors.Is(err, projects.ErrProgramNotFound),
		errors.Is(err, projects.ErrProgramOrgMismatch):
		return err
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "23505" && strings.Contains(pgErr.ConstraintName, "slug"):
			return projects.ErrSlugTaken
		case pgErr.Code == "23503" && strings.Contains(pgErr.ConstraintName, "program"):
			return projects.ErrProgramNotFound
		case pgErr.Code == "23503" && strings.Contains(pgErr.ConstraintName, "organization"):
			return projects.ErrOrgNotFound
		}
	}
	return fmt.Errorf("persistence: write project: %w", err)
}

// projectFromRow converts a sqlc project row to the domain value.
func projectFromRow(row sqlc.Project) domain.Project {
	return domain.Project{
		ID:                      pgUUIDToText(row.ID),
		OrganizationID:          pgUUIDTextPtr(row.OrganizationID),
		ProgramID:               pgUUIDTextPtr(row.ProgramID),
		Slug:                    row.Slug,
		Name:                    row.Name,
		Purpose:                 row.Purpose,
		ActivityStatus:          row.ActivityStatus,
		Visibility:              domain.ProjectVisibility(row.Visibility),
		MainFrozen:              row.MainFrozen,
		GitRepositoryExternalID: row.GitRepositoryExternalID,
		CreatedBy:               pgUUIDToText(row.CreatedBy),
		CreatedAt:               row.CreatedAt.Time,
		ProvisionStatus:         domain.ProvisionStatus(row.ProvisionStatus),
	}
}

// projectMembershipFromRow converts a sqlc membership row to the domain
// value.
func projectMembershipFromRow(row sqlc.ProjectMembership) domain.ProjectMembership {
	return domain.ProjectMembership{
		ProjectID: pgUUIDToText(row.ProjectID),
		UserID:    pgUUIDToText(row.UserID),
		Role:      domain.ProjectRole(row.Role),
		CreatedAt: row.CreatedAt.Time,
	}
}

// programFromRow converts a sqlc program row to the domain value.
func programFromRow(row sqlc.Program) domain.Program {
	return domain.Program{
		ID:             pgUUIDToText(row.ID),
		OrganizationID: pgUUIDTextPtr(row.OrganizationID),
		Slug:           row.Slug,
		Name:           row.Name,
		Description:    nullStringToValue(row.Description),
		CreatedAt:      row.CreatedAt.Time,
	}
}

// pgUUIDTextPtr converts a nullable pgx uuid to *string (nil when NULL).
func pgUUIDTextPtr(u pgtype.UUID) *string {
	if !u.Valid {
		return nil
	}
	s := pgUUIDToText(u)
	return &s
}

var (
	_ projects.ProjectStore = (*ProjectStore)(nil)
	_ projects.OrgGate      = (*OrgStore)(nil)
)
