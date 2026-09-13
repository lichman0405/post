package projects

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/lichman0405/post/internal/application/orgs"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
)

// Service orchestrates the project-creation shell (T0104), enforces
// the project permission matrix (T0105) and isolates reads by visibility
// (T0106). All policy lives here; the transport layer only translates
// requests into these calls. The rules:
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
//     projects) — conflicts answer ErrSlugTaken with a clear code;
//   - every public action passes an explicit authorization check through
//     the policy engine (docs/50, T0105): Create asks create_project;
//     reads ask the visibility-aware action (T0106): public projects ask
//     read_public_project (anonymous allowed), private projects ask
//     read_private_project (membership role decides). A denied (or
//     unresolved) decision refuses the action server-side — hiding a
//     control in a client never substitutes for this check;
//   - a denied read answers ErrProjectNotFound for members and outsiders
//     alike: the project's existence is never disclosed to a caller who
//     may not see it (read existence hiding, docs/45 — the same shape an
//     unknown project produces);
//   - the list is the union of public projects (visible to every matrix
//     class) and the caller's own projects (membership), newest first —
//     the query is the visibility filter, so a private project can only
//     appear for one of its members (T0106 L1: the docs name no explicit
//     list semantics beyond "anonymous must not see private"; the union
//     follows read_public_project, which allows every class, and
//     docs/31 Gate D "Explore/Search 可发现 public project").
type Service struct {
	store ProjectStore
	orgs  OrgGate
	authz authz.Engine
}

// NewService wires the service on the store, the organization gate and
// the policy engine. The engine is the one authority on project
// permissions; pass authz.NewMatrixEngine() in production.
func NewService(store ProjectStore, gate OrgGate, engine authz.Engine) *Service {
	return &Service{store: store, orgs: gate, authz: engine}
}

// maxPurpose bounds the free-text purpose (generous but finite — a hostile
// client must not be able to store megabytes per project).
const maxPurpose = 4000

// Create registers a new project and makes the actor its owner. The
// project comes back provision-pending: the row exists, the GitProvider
// repository does not yet (T0301).
func (s *Service) Create(ctx context.Context, actor domain.User, in CreateProjectInput) (domain.Project, domain.ProjectMembership, error) {
	// Explicit authorization first (docs/50): the matrix allows
	// create_project for any authenticated actor (the caller has no
	// project membership yet — the organization gate below adds the
	// per-organization rule). A denied decision refuses the write
	// regardless of what any client UI chose to render.
	if err := s.require(ctx, authz.Request{
		Action: authz.ActionCreateProject,
		Class:  authz.ClassOf(true, nil, false),
	}); err != nil {
		return domain.Project{}, domain.ProjectMembership{}, err
	}
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

// Get returns the project when the caller may read it, and hides its
// existence otherwise (T0106). The read passes an explicit policy-engine
// check with the visibility-aware action: a public project asks
// read_public_project — allowed for every matrix class, anonymous
// included; a private project asks read_private_project, where the
// actor's membership role resolves the matrix column. A denied decision
// — non-members and anonymous callers included — answers
// ErrProjectNotFound, so the project's existence (and every metadata
// field) is not disclosed to a caller who may not see it (read existence
// hiding, docs/45). Store failures answer ErrStore (503), never a masked
// 404.
func (s *Service) Get(ctx context.Context, r Reader, projectID string) (domain.Project, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return domain.Project{}, wrapStoreError(err)
	}
	role, err := s.membershipRole(ctx, projectID, r.UserID)
	if err != nil {
		return domain.Project{}, wrapStoreError(err)
	}
	if err := s.requireRead(ctx, project, r, role); err != nil {
		if errors.Is(err, ErrForbidden) {
			// Read existence hiding: a denied read looks exactly like an
			// unknown project (docs/45; the same 404 shape as the org
			// surface).
			return domain.Project{}, ErrProjectNotFound
		}
		return domain.Project{}, err
	}
	return project, nil
}

// GetMembership returns the actor's own membership in the project (the
// shell's permission-aware Settings gate reads this; T0109's member
// management builds on the same store method). The project-read
// authorization runs first — a caller who may not read the project may
// not learn their membership in it either (the same existence hiding as
// Get, and the same public-read extension when T0106 lands). For a
// project the caller may read but is not a member of, the answer is
// ErrMemberNotFound — "no role", not an error.
//
// T0106 landed: Get now takes a Reader, so the read authorization below is
// the one-liner its own comment anticipated.
func (s *Service) GetMembership(ctx context.Context, actor domain.User, projectID string) (domain.ProjectMembership, error) {
	if _, err := s.Get(ctx, Reader{UserID: actor.ID, Authenticated: true}, projectID); err != nil {
		return domain.ProjectMembership{}, err
	}
	membership, err := s.store.GetMembership(ctx, projectID, actor.ID)
	if err != nil {
		return domain.ProjectMembership{}, wrapStoreError(err)
	}
	return membership, nil
}

// requireRead picks the visibility-aware matrix action for one read and
// evaluates it: public projects ask read_public_project (every class
// allowed), private projects ask read_private_project (the membership
// role decides — anonymous and non-member classes deny).
func (s *Service) requireRead(ctx context.Context, p domain.Project, r Reader, role *domain.ProjectRole) error {
	action := authz.ActionReadPrivateProject
	if p.Visibility == domain.VisibilityPublic {
		action = authz.ActionReadPublicProject
	}
	return s.require(ctx, authz.Request{
		Action: action,
		Class:  authz.ClassOf(r.Authenticated, role, false),
	})
}

// List returns the projects the caller may see, newest first: every
// public project (readable by all matrix classes, T0106) plus, for an
// authenticated caller, the projects they belong to at any role. A
// private project therefore only ever appears for one of its members —
// the store queries are the filter, and both halves of the union are
// visibility-filtered before they reach the caller.
func (s *Service) List(ctx context.Context, r Reader) ([]domain.Project, error) {
	public, err := s.store.ListPublicProjects(ctx)
	if err != nil {
		return nil, wrapStoreError(err)
	}
	var own []domain.Project
	if r.Authenticated {
		own, err = s.store.ListProjectsForUser(ctx, r.UserID)
		if err != nil {
			return nil, wrapStoreError(err)
		}
	}
	return mergeNewestFirst(public, own), nil
}

// mergeNewestFirst unions two project lists (the caller's own projects
// may already be in the public half), dropping duplicates and ordering by
// creation time, newest first (id as the deterministic tie-breaker).
func mergeNewestFirst(lists ...[]domain.Project) []domain.Project {
	seen := make(map[string]bool)
	var out []domain.Project
	for _, list := range lists {
		for _, p := range list {
			if seen[p.ID] {
				continue
			}
			seen[p.ID] = true
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b domain.Project) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// membershipRole returns the actor's project membership role, or nil
// when they hold no membership. A store failure (as opposed to "no
// row") is returned as an error — the difference decides between an
// existence-hidden 404 and a fail-closed 503.
func (s *Service) membershipRole(ctx context.Context, projectID, userID string) (*domain.ProjectRole, error) {
	membership, err := s.store.GetMembership(ctx, projectID, userID)
	switch {
	case err == nil:
		return &membership.Role, nil
	case errors.Is(err, ErrMemberNotFound):
		return nil, nil
	default:
		return nil, err
	}
}

// require evaluates one authorization request and fails closed. A
// permitting verdict passes; every other verdict — an outright deny, or
// a conditional form this site does not resolve — answers ErrForbidden;
// an engine failure (or a service wired without an engine) answers
// ErrStore: state is unknowable, so the action is refused rather than
// guessed (default deny, docs/12).
func (s *Service) require(ctx context.Context, req authz.Request) error {
	if s.authz == nil {
		return fmt.Errorf("%w: no policy engine configured", ErrStore)
	}
	decision, err := s.authz.Authorize(ctx, req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrStore, err)
	}
	if !decision.Permits() {
		return ErrForbidden
	}
	return nil
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
		errors.Is(err, ErrMemberNotFound),
		errors.Is(err, ErrTargetMemberNotFound),
		errors.Is(err, ErrLastOwner),
		errors.Is(err, ErrSettingsForbidden):
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
