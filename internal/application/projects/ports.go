package projects

import (
	"context"

	"github.com/lichman0405/post/internal/domain"
)

// Ports: the project store is the application's one gateway to canonical
// project state (docs/52: application orchestrates against ports; adapters
// live in internal/persistence). Multi-step writes (create project +
// creator's owner membership) are one atomic store call each, so the
// invariants hold under concurrency. The organization gate is a separate
// port because organization state is owned by the orgs application
// (T0103); the production adapter is persistence.OrgStore, which satisfies
// it structurally.

// CreateProjectInput carries the client's creation request after transport
// decoding. Slug/Name/Purpose are the raw values — normalization and
// validation happen in the service.
type CreateProjectInput struct {
	// OrganizationID names the owning organization; nil creates a personal
	// project (the canonical schema and the OpenAPI contract allow null).
	OrganizationID *string
	// ProgramID optionally groups the project under an existing program of
	// the same organization. Requires OrganizationID (V1: no programs on
	// personal projects).
	ProgramID  *string
	Slug       string
	Name       string
	Purpose    string
	Visibility domain.ProjectVisibility
}

// ProjectStore is the persistence port for projects, programs and project
// memberships.
type ProjectStore interface {
	// CreateProject creates the project and the creator's owner membership
	// in one transaction. The project is created provision-pending (the
	// canonical default). It fails with ErrSlugTaken when the slug is
	// taken, ErrOrgNotFound for an unknown organization, ErrOrgDeactivated
	// for a deactivated one, ErrProgramNotFound for an unknown program,
	// ErrProgramOrgMismatch when the program belongs elsewhere, and
	// ErrForbidden when the creator is not an active member of the
	// organization (re-checked inside the transaction, so a membership
	// change racing the create cannot slip through).
	CreateProject(ctx context.Context, p domain.Project, creatorUserID string) (domain.Project, domain.ProjectMembership, error)
	// GetProject returns the project or ErrProjectNotFound.
	GetProject(ctx context.Context, projectID string) (domain.Project, error)
	// GetMembership returns one project membership or ErrMemberNotFound.
	GetMembership(ctx context.Context, projectID, userID string) (domain.ProjectMembership, error)
	// ListProjectsForUser returns the projects the user belongs to
	// (any project membership), newest first.
	ListProjectsForUser(ctx context.Context, userID string) ([]domain.Project, error)
	// GetProgram returns the program or ErrProgramNotFound.
	GetProgram(ctx context.Context, programID string) (domain.Program, error)
}

// OrgGate is the slice of organization state the project service needs:
// the organization a project is created into must exist, be active, and
// the creator must be an active member of it (T0104 L1 — recorded for the
// Supervisor: the docs do not spell out who may create projects inside an
// organization; active membership is the minimal defensible rule).
//
// The adapter answers with the orgs application's sentinels
// (orgs.ErrOrgNotFound / orgs.ErrMemberNotFound), which the service maps
// onto its own.
type OrgGate interface {
	// GetOrganization returns the organization or orgs.ErrOrgNotFound.
	GetOrganization(ctx context.Context, orgID string) (domain.Organization, error)
	// GetMembership returns the actor's membership or
	// orgs.ErrMemberNotFound.
	GetMembership(ctx context.Context, orgID, userID string) (domain.OrganizationMembership, error)
}
