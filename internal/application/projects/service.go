package projects

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/lichman0405/post/internal/application/orgs"
	"github.com/lichman0405/post/internal/domain"
)

// Service orchestrates the project-creation shell (T0104). All policy
// lives here; the transport layer only translates requests into these
// calls. The rules:
//
//   - creating a project makes the creator its owner;
//   - every new project starts provision-pending (provision_status
//     'pending') — the GitProvider repository arrives with T0301;
//   - visibility is a Public/Private preset chosen at creation;
//   - purpose is required; program is optional and must belong to the
//     same organization (V1: no programs on personal projects);
//   - creating a project inside an organization requires an active
//     membership in it (L1 decision, see ports.OrgGate);
//   - personal projects (no organization) need no gate;
//   - the slug is unique inside the organization (globally for personal
//     projects) — conflicts answer ErrSlugTaken with a clear code.
type Service struct {
	store ProjectStore
	orgs  OrgGate
}

// NewService wires the service on the store and organization gate ports.
func NewService(store ProjectStore, gate OrgGate) *Service {
	return &Service{store: store, orgs: gate}
}

// maxPurpose bounds the free-text purpose (generous but finite — a hostile
// client must not be able to store megabytes per project).
const maxPurpose = 4000

// Create registers a new project and makes the actor its owner. The
// project comes back provision-pending: the row exists, the GitProvider
// repository does not yet (T0301).
func (s *Service) Create(ctx context.Context, actor domain.User, in CreateProjectInput) (domain.Project, domain.ProjectMembership, error) {
	if !domain.ValidProjectSlug(in.Slug) {
		return domain.Project{}, domain.ProjectMembership{}, fmt.Errorf("%w: slug may only contain lowercase letters, digits and dashes (max 64 characters)", ErrValidation)
	}
	if !domain.ValidProjectName(in.Name) {
		return domain.Project{}, domain.ProjectMembership{}, fmt.Errorf("%w: name is required (max 200 characters)", ErrValidation)
	}
	if !domain.ValidProjectPurpose(in.Purpose) {
		return domain.Project{}, domain.ProjectMembership{}, fmt.Errorf("%w: purpose is required (max 4000 characters)", ErrValidation)
	}
	if !domain.ValidProjectVisibility(in.Visibility) {
		return domain.Project{}, domain.ProjectMembership{}, fmt.Errorf("%w: visibility must be public or private", ErrValidation)
	}
	// An empty-string id is refused, never silently interpreted: turning
	// "" into "no organization" would create a personal project where the
	// client meant an organization (visibility semantics differ).
	if in.OrganizationID != nil && *in.OrganizationID == "" {
		return domain.Project{}, domain.ProjectMembership{}, fmt.Errorf("%w: organization_id must be null or a valid id", ErrValidation)
	}
	if in.ProgramID != nil && *in.ProgramID == "" {
		return domain.Project{}, domain.ProjectMembership{}, fmt.Errorf("%w: program_id must be null or a valid id", ErrValidation)
	}
	if in.OrganizationID == nil && in.ProgramID != nil {
		return domain.Project{}, domain.ProjectMembership{}, fmt.Errorf("%w: a program requires an organization", ErrValidation)
	}

	if in.OrganizationID != nil {
		if err := s.requireActiveOrgMember(ctx, *in.OrganizationID, actor.ID); err != nil {
			return domain.Project{}, domain.ProjectMembership{}, err
		}
		if in.ProgramID != nil {
			if err := s.requireOrgProgram(ctx, *in.OrganizationID, *in.ProgramID); err != nil {
				return domain.Project{}, domain.ProjectMembership{}, err
			}
		}
	}

	p := domain.Project{
		OrganizationID:  in.OrganizationID,
		ProgramID:       in.ProgramID,
		Slug:            domain.NormalizeProjectSlug(in.Slug),
		Name:            strings.TrimSpace(in.Name),
		Purpose:         trimTo(in.Purpose, maxPurpose),
		Visibility:      in.Visibility,
		ActivityStatus:  "planning",
		ProvisionStatus: domain.ProvisionPending,
	}
	created, membership, err := s.store.CreateProject(ctx, p, actor.ID)
	if err != nil {
		return domain.Project{}, domain.ProjectMembership{}, wrapStoreError(err)
	}
	return created, membership, nil
}

// Get returns the project for a current member. Non-members (and unknown
// projects) answer ErrProjectNotFound — the project's existence is not
// disclosed to outsiders (read existence hiding; T0106 extends reads to
// public projects). Store failures answer ErrStore (503), never a masked
// 404.
func (s *Service) Get(ctx context.Context, actor domain.User, projectID string) (domain.Project, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return domain.Project{}, wrapStoreError(err)
	}
	if _, err := s.store.GetMembership(ctx, projectID, actor.ID); err != nil {
		if errors.Is(err, ErrMemberNotFound) {
			return domain.Project{}, ErrProjectNotFound
		}
		return domain.Project{}, wrapStoreError(err)
	}
	return project, nil
}

// List returns the projects the actor belongs to (any role), newest first.
func (s *Service) List(ctx context.Context, actor domain.User) ([]domain.Project, error) {
	projects, err := s.store.ListProjectsForUser(ctx, actor.ID)
	if err != nil {
		return nil, wrapStoreError(err)
	}
	return projects, nil
}

// requireActiveOrgMember enforces the organization gate: the organization
// must exist and be active, and the actor must hold an active membership
// in it.
func (s *Service) requireActiveOrgMember(ctx context.Context, orgID, actorID string) error {
	org, err := s.orgs.GetOrganization(ctx, orgID)
	if err != nil {
		return wrapGateError(err)
	}
	if !org.Active() {
		return ErrOrgDeactivated
	}
	m, err := s.orgs.GetMembership(ctx, orgID, actorID)
	if err != nil {
		return wrapGateError(err)
	}
	if !m.Active() {
		return ErrForbidden
	}
	return nil
}

// requireOrgProgram verifies the program reference: it must exist and
// belong to the same organization as the project.
func (s *Service) requireOrgProgram(ctx context.Context, orgID, programID string) error {
	program, err := s.store.GetProgram(ctx, programID)
	if err != nil {
		return wrapStoreError(err)
	}
	if program.OrganizationID == nil || *program.OrganizationID != orgID {
		return ErrProgramOrgMismatch
	}
	return nil
}

// trimTo trims and length-bounds a free-text field. Truncation happens on
// rune boundaries — slicing bytes could split a multi-byte rune and store
// invalid UTF-8.
func trimTo(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	r := []rune(s)
	if len(r) > max {
		s = string(r[:max])
	}
	return s
}

// wrapStoreError maps the store's sentinel errors onto the service's
// (they share the names by design, but the store contract is explicit
// about which errors each method may return).
func wrapStoreError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrProjectNotFound),
		errors.Is(err, ErrSlugTaken),
		errors.Is(err, ErrForbidden),
		errors.Is(err, ErrOrgNotFound),
		errors.Is(err, ErrOrgDeactivated),
		errors.Is(err, ErrProgramNotFound),
		errors.Is(err, ErrProgramOrgMismatch),
		errors.Is(err, ErrMemberNotFound):
		return err
	default:
		return fmt.Errorf("%w: %v", ErrStore, err)
	}
}

// wrapGateError maps the organization gate's sentinels (orgs application)
// onto this package's: an unknown organization stays ErrOrgNotFound; a
// missing membership is a permission problem here, not a "not found" —
// the actor simply may not create projects in that organization.
func wrapGateError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, orgs.ErrOrgNotFound):
		return ErrOrgNotFound
	case errors.Is(err, orgs.ErrMemberNotFound):
		return ErrForbidden
	case errors.Is(err, orgs.ErrStore):
		return fmt.Errorf("%w: %v", ErrStore, err)
	default:
		return fmt.Errorf("%w: %v", ErrStore, err)
	}
}
