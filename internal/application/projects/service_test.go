package projects

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/orgs"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
)

// fakeStore is an in-memory ProjectStore for the service policy tests; the
// organization gate lives in a separate fakeGate (the two ports share a
// GetMembership name with different shapes, so no single type can implement
// both). The store reproduces the adapter's invariants (org gate, program
// belonging, per-scope slug uniqueness, owner membership) so the service
// rules can be tested without PostgreSQL. The real adapter is exercised by
// the integration suite against the real database.
type fakeStore struct {
	projects map[string]domain.Project
	members  map[[2]string]domain.ProjectMembership
	programs map[string]domain.Program
	nextID   int
	// gate is the organization state the store re-checks inside
	// CreateProject, mirroring the production transaction.
	gate *fakeGate
	// failWith, when set, makes CreateProject return it (wrapped by the
	// service as ErrStore).
	failWith error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		projects: map[string]domain.Project{},
		members:  map[[2]string]domain.ProjectMembership{},
		programs: map[string]domain.Program{},
		gate:     newFakeGate(),
	}
}

func (s *fakeStore) freshID() string {
	s.nextID++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", s.nextID)
}

// seedOrg registers an active organization and an active owner membership
// in the gate.
func (s *fakeStore) seedOrg(orgID, ownerID string) { s.gate.seedOrg(orgID, ownerID) }

// seedMember writes one project membership row directly (the production
// member-management API arrives with T0109; T0105 tests seed state to
// exercise the role columns of the permission matrix).
func (s *fakeStore) seedMember(projectID, userID string, role domain.ProjectRole) {
	s.members[[2]string{projectID, userID}] = domain.ProjectMembership{
		ProjectID: projectID, UserID: userID, Role: role,
	}
}

func (s *fakeStore) seedProgram(programID, orgID string) {
	s.programs[programID] = domain.Program{ID: programID, OrganizationID: &orgID, Slug: "prog-" + programID, Name: "Program " + programID}
}

func (s *fakeStore) CreateProject(ctx context.Context, p domain.Project, creatorUserID string) (domain.Project, domain.ProjectMembership, error) {
	if s.failWith != nil {
		return domain.Project{}, domain.ProjectMembership{}, s.failWith
	}
	if p.OrganizationID != nil {
		org, err := s.gate.GetOrganization(ctx, *p.OrganizationID)
		if err != nil {
			return domain.Project{}, domain.ProjectMembership{}, ErrOrgNotFound
		}
		if !org.Active() {
			return domain.Project{}, domain.ProjectMembership{}, ErrOrgDeactivated
		}
		m, err := s.gate.GetMembership(ctx, *p.OrganizationID, creatorUserID)
		if err != nil || !m.Active() {
			return domain.Project{}, domain.ProjectMembership{}, ErrForbidden
		}
	}
	if p.ProgramID != nil {
		prog, ok := s.programs[*p.ProgramID]
		if !ok {
			return domain.Project{}, domain.ProjectMembership{}, ErrProgramNotFound
		}
		if prog.OrganizationID == nil || p.OrganizationID == nil || *prog.OrganizationID != *p.OrganizationID {
			return domain.Project{}, domain.ProjectMembership{}, ErrProgramOrgMismatch
		}
	}
	for _, existing := range s.projects {
		if existing.Slug != p.Slug {
			continue
		}
		if (existing.OrganizationID == nil && p.OrganizationID == nil) ||
			(existing.OrganizationID != nil && p.OrganizationID != nil && *existing.OrganizationID == *p.OrganizationID) {
			return domain.Project{}, domain.ProjectMembership{}, ErrSlugTaken
		}
	}
	p.ID = s.freshID()
	s.projects[p.ID] = p
	m := domain.ProjectMembership{ProjectID: p.ID, UserID: creatorUserID, Role: domain.ProjectRoleOwner}
	s.members[[2]string{p.ID, creatorUserID}] = m
	return p, m, nil
}

func (s *fakeStore) GetProject(ctx context.Context, projectID string) (domain.Project, error) {
	p, ok := s.projects[projectID]
	if !ok {
		return domain.Project{}, ErrProjectNotFound
	}
	return p, nil
}

func (s *fakeStore) GetMembership(ctx context.Context, projectID, userID string) (domain.ProjectMembership, error) {
	m, ok := s.members[[2]string{projectID, userID}]
	if !ok {
		return domain.ProjectMembership{}, ErrMemberNotFound
	}
	return m, nil
}

func (s *fakeStore) ListProjectsForUser(ctx context.Context, userID string) ([]domain.Project, error) {
	out := []domain.Project{}
	for _, m := range s.members {
		if m.UserID == userID {
			out = append(out, s.projects[m.ProjectID])
		}
	}
	return out, nil
}

func (s *fakeStore) GetProgram(ctx context.Context, programID string) (domain.Program, error) {
	p, ok := s.programs[programID]
	if !ok {
		return domain.Program{}, ErrProgramNotFound
	}
	return p, nil
}

// fakeGate is an in-memory OrgGate answering with the orgs application's
// sentinels, exactly like the production adapter (persistence.OrgStore).
type fakeGate struct {
	orgs    map[string]domain.Organization
	members map[[2]string]domain.OrganizationMembership
}

func newFakeGate() *fakeGate {
	return &fakeGate{
		orgs:    map[string]domain.Organization{},
		members: map[[2]string]domain.OrganizationMembership{},
	}
}

func (g *fakeGate) seedOrg(orgID, ownerID string) {
	g.orgs[orgID] = domain.Organization{ID: orgID, Slug: "org-" + orgID, Name: "Org " + orgID}
	g.members[[2]string{orgID, ownerID}] = domain.OrganizationMembership{
		OrganizationID: orgID, UserID: ownerID, Role: domain.OrgRoleOwner, Verified: true,
	}
}

func (g *fakeGate) GetOrganization(ctx context.Context, orgID string) (domain.Organization, error) {
	o, ok := g.orgs[orgID]
	if !ok {
		return domain.Organization{}, orgs.ErrOrgNotFound
	}
	return o, nil
}

func (g *fakeGate) GetMembership(ctx context.Context, orgID, userID string) (domain.OrganizationMembership, error) {
	m, ok := g.members[[2]string{orgID, userID}]
	if !ok {
		return domain.OrganizationMembership{}, orgs.ErrMemberNotFound
	}
	return m, nil
}

var (
	_ ProjectStore = (*fakeStore)(nil)
	_ OrgGate      = (*fakeGate)(nil)
)

func testUser(id string) domain.User { return domain.User{ID: id, Handle: "u-" + id} }

func TestCreatePersonalProject(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	svc := NewService(store, store.gate, authz.NewMatrixEngine())

	project, membership, err := svc.Create(ctx, testUser("alice"), CreateProjectInput{
		Slug: "MOF-Lab", Name: "MOF Lab", Purpose: "screen MOFs", Visibility: domain.VisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if project.Slug != "mof-lab" {
		t.Errorf("slug = %q, want normalized mof-lab", project.Slug)
	}
	if project.OrganizationID != nil {
		t.Errorf("organization_id = %v, want nil", project.OrganizationID)
	}
	if project.Visibility != domain.VisibilityPrivate {
		t.Errorf("visibility = %q, want private", project.Visibility)
	}
	if project.ProvisionStatus != domain.ProvisionPending {
		t.Errorf("provision_status = %q, want pending", project.ProvisionStatus)
	}
	if membership.Role != domain.ProjectRoleOwner || membership.UserID != "alice" || membership.ProjectID != project.ID {
		t.Errorf("membership = %+v, want alice's owner membership of %s", membership, project.ID)
	}
}

func TestCreateRejectsInvalidInput(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	svc := NewService(store, store.gate, authz.NewMatrixEngine())
	base := CreateProjectInput{
		Slug: "ok-slug", Name: "OK", Purpose: "purpose", Visibility: domain.VisibilityPublic,
	}
	cases := []struct {
		name  string
		mut   func(*CreateProjectInput)
		match error
	}{
		{"bad slug", func(in *CreateProjectInput) { in.Slug = "Has Spaces" }, ErrValidation},
		{"empty name", func(in *CreateProjectInput) { in.Name = "  " }, ErrValidation},
		{"missing purpose", func(in *CreateProjectInput) { in.Purpose = "" }, ErrValidation},
		{"bad visibility", func(in *CreateProjectInput) { in.Visibility = "internal" }, ErrValidation},
		{"program without org", func(in *CreateProjectInput) { id := "p1"; in.ProgramID = &id }, ErrValidation},
		{"empty org id", func(in *CreateProjectInput) { id := ""; in.OrganizationID = &id }, ErrValidation},
		{"empty program id", func(in *CreateProjectInput) { id := ""; in.ProgramID = &id }, ErrValidation},
	}
	for _, tc := range cases {
		in := base
		tc.mut(&in)
		if _, _, err := svc.Create(ctx, testUser("alice"), in); !errors.Is(err, tc.match) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.match)
		}
	}
}

func TestCreatePersonalSlugConflict(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	svc := NewService(store, store.gate, authz.NewMatrixEngine())
	if _, _, err := svc.Create(ctx, testUser("alice"), CreateProjectInput{
		Slug: "solo", Name: "Solo", Purpose: "x", Visibility: domain.VisibilityPublic,
	}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, _, err := svc.Create(ctx, testUser("alice"), CreateProjectInput{
		Slug: "SOLO", Name: "Solo 2", Purpose: "x", Visibility: domain.VisibilityPublic,
	}); !errors.Is(err, ErrSlugTaken) {
		t.Errorf("second create = %v, want ErrSlugTaken (normalization collision)", err)
	}
}

func TestCreateInOrgGate(t *testing.T) {
	ctx := context.Background()
	orgID := "org-1"

	t.Run("active member creates", func(t *testing.T) {
		store := newFakeStore()
		store.seedOrg(orgID, "alice")
		svc := NewService(store, store.gate, authz.NewMatrixEngine())
		project, _, err := svc.Create(ctx, testUser("alice"), CreateProjectInput{
			OrganizationID: &orgID, Slug: "team-project", Name: "Team", Purpose: "x",
			Visibility: domain.VisibilityPublic,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if project.OrganizationID == nil || *project.OrganizationID != orgID {
			t.Errorf("organization_id = %v, want %s", project.OrganizationID, orgID)
		}
	})

	t.Run("unknown org", func(t *testing.T) {
		store := newFakeStore()
		svc := NewService(store, store.gate, authz.NewMatrixEngine())
		if _, _, err := svc.Create(ctx, testUser("alice"), CreateProjectInput{
			OrganizationID: &orgID, Slug: "p", Name: "P", Purpose: "x", Visibility: domain.VisibilityPublic,
		}); !errors.Is(err, ErrOrgNotFound) {
			t.Errorf("err = %v, want ErrOrgNotFound", err)
		}
	})

	t.Run("non-member", func(t *testing.T) {
		store := newFakeStore()
		store.seedOrg(orgID, "alice")
		svc := NewService(store, store.gate, authz.NewMatrixEngine())
		if _, _, err := svc.Create(ctx, testUser("bob"), CreateProjectInput{
			OrganizationID: &orgID, Slug: "p", Name: "P", Purpose: "x", Visibility: domain.VisibilityPublic,
		}); !errors.Is(err, ErrForbidden) {
			t.Errorf("err = %v, want ErrForbidden", err)
		}
	})

	t.Run("deactivated org", func(t *testing.T) {
		store := newFakeStore()
		store.seedOrg(orgID, "alice")
		o := store.gate.orgs[orgID]
		past := o.CreatedAt.Add(-time.Hour)
		o.DeactivatedAt = &past
		store.gate.orgs[orgID] = o
		svc := NewService(store, store.gate, authz.NewMatrixEngine())
		if _, _, err := svc.Create(ctx, testUser("alice"), CreateProjectInput{
			OrganizationID: &orgID, Slug: "p", Name: "P", Purpose: "x", Visibility: domain.VisibilityPublic,
		}); !errors.Is(err, ErrOrgDeactivated) {
			t.Errorf("err = %v, want ErrOrgDeactivated", err)
		}
	})
}

func TestCreateWithProgram(t *testing.T) {
	ctx := context.Background()
	orgID := "org-1"

	t.Run("program of the same org", func(t *testing.T) {
		store := newFakeStore()
		store.seedOrg(orgID, "alice")
		store.seedProgram("prog-1", orgID)
		svc := NewService(store, store.gate, authz.NewMatrixEngine())
		project, _, err := svc.Create(ctx, testUser("alice"), CreateProjectInput{
			OrganizationID: &orgID, ProgramID: strPtr("prog-1"),
			Slug: "p", Name: "P", Purpose: "x", Visibility: domain.VisibilityPublic,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if project.ProgramID == nil || *project.ProgramID != "prog-1" {
			t.Errorf("program_id = %v, want prog-1", project.ProgramID)
		}
	})

	t.Run("unknown program", func(t *testing.T) {
		store := newFakeStore()
		store.seedOrg(orgID, "alice")
		svc := NewService(store, store.gate, authz.NewMatrixEngine())
		if _, _, err := svc.Create(ctx, testUser("alice"), CreateProjectInput{
			OrganizationID: &orgID, ProgramID: strPtr("ghost"),
			Slug: "p", Name: "P", Purpose: "x", Visibility: domain.VisibilityPublic,
		}); !errors.Is(err, ErrProgramNotFound) {
			t.Errorf("err = %v, want ErrProgramNotFound", err)
		}
	})

	t.Run("program of another org", func(t *testing.T) {
		store := newFakeStore()
		store.seedOrg(orgID, "alice")
		store.seedProgram("prog-1", "org-2")
		svc := NewService(store, store.gate, authz.NewMatrixEngine())
		if _, _, err := svc.Create(ctx, testUser("alice"), CreateProjectInput{
			OrganizationID: &orgID, ProgramID: strPtr("prog-1"),
			Slug: "p", Name: "P", Purpose: "x", Visibility: domain.VisibilityPublic,
		}); !errors.Is(err, ErrProgramOrgMismatch) {
			t.Errorf("err = %v, want ErrProgramOrgMismatch", err)
		}
	})
}

func TestCreateRejectsOverlongPurpose(t *testing.T) {
	// A purpose over the bound is refused, never silently truncated — a
	// research-goal statement must not lose its ending.
	ctx := context.Background()
	store := newFakeStore()
	svc := NewService(store, store.gate, authz.NewMatrixEngine())
	_, _, err := svc.Create(ctx, testUser("alice"), CreateProjectInput{
		Slug: "p", Name: "P", Purpose: strings.Repeat("x", maxPurpose+1),
		Visibility: domain.VisibilityPublic,
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}

func TestGetHidesExistence(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	svc := NewService(store, store.gate, authz.NewMatrixEngine())
	project, _, err := svc.Create(ctx, testUser("alice"), CreateProjectInput{
		Slug: "p", Name: "P", Purpose: "x", Visibility: domain.VisibilityPublic,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Get(ctx, testUser("alice"), project.ID); err != nil {
		t.Errorf("owner Get: %v", err)
	}
	if _, err := svc.Get(ctx, testUser("bob"), project.ID); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("outsider Get = %v, want ErrProjectNotFound (existence hiding)", err)
	}
	if _, err := svc.Get(ctx, testUser("alice"), "no-such-id"); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("unknown Get = %v, want ErrProjectNotFound", err)
	}
}

func TestList(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	svc := NewService(store, store.gate, authz.NewMatrixEngine())
	if _, _, err := svc.Create(ctx, testUser("alice"), CreateProjectInput{
		Slug: "a", Name: "A", Purpose: "x", Visibility: domain.VisibilityPublic,
	}); err != nil {
		t.Fatalf("Create a: %v", err)
	}
	if _, _, err := svc.Create(ctx, testUser("bob"), CreateProjectInput{
		Slug: "b", Name: "B", Purpose: "x", Visibility: domain.VisibilityPublic,
	}); err != nil {
		t.Fatalf("Create b: %v", err)
	}
	aliceList, err := svc.List(ctx, testUser("alice"))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(aliceList) != 1 || aliceList[0].Slug != "a" {
		t.Errorf("alice list = %+v, want only project a", aliceList)
	}
	bobList, err := svc.List(ctx, testUser("bob"))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(bobList) != 1 || bobList[0].Slug != "b" {
		t.Errorf("bob list = %+v, want only project b", bobList)
	}
}

func TestStoreFailureWrapsAsErrStore(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	svc := NewService(store, store.gate, authz.NewMatrixEngine())
	store.failWith = errors.New("boom")
	if _, _, err := svc.Create(ctx, testUser("alice"), CreateProjectInput{
		Slug: "p", Name: "P", Purpose: "x", Visibility: domain.VisibilityPublic,
	}); !errors.Is(err, ErrStore) {
		t.Errorf("err = %v, want wrapped ErrStore", err)
	}
}

func strPtr(s string) *string { return &s }

// stubEngine is a test policy engine: fixed decision, fixed error.
type stubEngine struct {
	decision authz.Decision
	err      error
}

func (e *stubEngine) Authorize(ctx context.Context, req authz.Request) (authz.Decision, error) {
	return e.decision, e.err
}

// TestGetMembership (T0108): the shell's own-membership read resolves the
// actor's role from the store — the permission-aware Settings gate is
// driven by this data, never by a client claim.
func TestGetMembership(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	svc := NewService(store, store.gate, authz.NewMatrixEngine())
	project, created, err := svc.Create(ctx, testUser("alice"), CreateProjectInput{
		Slug: "p", Name: "P", Purpose: "x", Visibility: domain.VisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The creator reads their owner membership back.
	got, err := svc.GetMembership(ctx, testUser("alice"), project.ID)
	if err != nil {
		t.Fatalf("GetMembership(owner): %v", err)
	}
	if got.Role != created.Role || got.ProjectID != project.ID || got.UserID != "alice" {
		t.Errorf("GetMembership(owner) = %+v, want role %s on project %s for alice",
			got, created.Role, project.ID)
	}
	// A seeded viewer reads their own role — the same row the engine used
	// for the project read.
	store.seedMember(project.ID, "bob", domain.ProjectRoleViewer)
	got, err = svc.GetMembership(ctx, testUser("bob"), project.ID)
	if err != nil {
		t.Fatalf("GetMembership(viewer): %v", err)
	}
	if got.Role != domain.ProjectRoleViewer {
		t.Errorf("GetMembership(viewer) role = %q, want viewer", got.Role)
	}
}

// TestGetMembershipHidesExistence: a caller who may not read the project
// may not learn their membership in it — the same existence-hiding shape
// as Get (in the current member-only read policy a non-member's
// membership read of any project answers ErrProjectNotFound).
func TestGetMembershipHidesExistence(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	svc := NewService(store, store.gate, authz.NewMatrixEngine())
	project, _, err := svc.Create(ctx, testUser("alice"), CreateProjectInput{
		Slug: "p", Name: "P", Purpose: "x", Visibility: domain.VisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.GetMembership(ctx, testUser("outsider"), project.ID); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("outsider GetMembership = %v, want ErrProjectNotFound (existence hiding)", err)
	}
	if _, err := svc.GetMembership(ctx, testUser("alice"), "no-such-id"); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("unknown GetMembership = %v, want ErrProjectNotFound", err)
	}
}

// TestGetMembershipNoRoleWhenReadable: when the project read is permitted
// but the actor holds no membership (the public-read policy T0106 brings
// for public projects), the answer is ErrMemberNotFound — "no role", which
// the shell renders as no Settings tab, not as an error. The permissive
// engine stands in for the visibility-aware read so the branch is
// testable before T0106 lands.
func TestGetMembershipNoRoleWhenReadable(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	allowAll := &stubEngine{decision: authz.Decision{Verdict: authz.VerdictAllow}}
	svc := NewService(store, store.gate, allowAll)
	project, _, err := svc.Create(ctx, testUser("alice"), CreateProjectInput{
		Slug: "p", Name: "P", Purpose: "x", Visibility: domain.VisibilityPublic,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.GetMembership(ctx, testUser("bob"), project.ID); !errors.Is(err, ErrMemberNotFound) {
		t.Errorf("nonmember GetMembership under permissive read = %v, want ErrMemberNotFound", err)
	}
}

// TestGetRoleMatrix (T0105): the read of a private project passes an
// explicit engine check per role — viewer, contributor, maintainer and
// owner all read; a non-member is refused with the existence-hiding 404
// shape. The four role columns are the server-side authorization, not a
// client-side visibility choice.
func TestGetRoleMatrix(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	svc := NewService(store, store.gate, authz.NewMatrixEngine())
	project, _, err := svc.Create(ctx, testUser("alice"), CreateProjectInput{
		Slug: "p", Name: "P", Purpose: "x", Visibility: domain.VisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	roles := []domain.ProjectRole{
		domain.ProjectRoleViewer,
		domain.ProjectRoleContributor,
		domain.ProjectRoleMaintainer,
		domain.ProjectRoleOwner,
	}
	for _, role := range roles {
		store.seedMember(project.ID, "bob", role)
		if _, err := svc.Get(ctx, testUser("bob"), project.ID); err != nil {
			t.Errorf("Get as %s: %v, want nil", role, err)
		}
	}
	if _, err := svc.Get(ctx, testUser("outsider"), project.ID); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("outsider Get = %v, want ErrProjectNotFound (existence hiding)", err)
	}
	// Defense in depth: a membership row with a role outside the four
	// canonical ones resolves to no matrix class and denies.
	store.seedMember(project.ID, "bogus", domain.ProjectRole("admin"))
	if _, err := svc.Get(ctx, testUser("bogus"), project.ID); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("unknown-role Get = %v, want ErrProjectNotFound (default deny)", err)
	}
}

// TestServiceEngineDenied: when the policy engine denies, the service
// refuses — Create answers ErrForbidden, Get keeps the existence-hiding
// shape. The denial originates server-side in the engine; no client can
// opt out of it by calling the API differently.
func TestServiceEngineDenied(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	deny := &stubEngine{decision: authz.Decision{Verdict: authz.VerdictDeny}}
	svc := NewService(store, store.gate, deny)
	if _, _, err := svc.Create(ctx, testUser("alice"), CreateProjectInput{
		Slug: "p", Name: "P", Purpose: "x", Visibility: domain.VisibilityPublic,
	}); !errors.Is(err, ErrForbidden) {
		t.Errorf("Create under deny = %v, want ErrForbidden", err)
	}
	// Get with an engine that denies (even for a member) masks to the
	// same 404 shape outsiders see.
	okSvc := NewService(store, store.gate, authz.NewMatrixEngine())
	project, _, err := okSvc.Create(ctx, testUser("alice"), CreateProjectInput{
		Slug: "p", Name: "P", Purpose: "x", Visibility: domain.VisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Get(ctx, testUser("alice"), project.ID); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("member Get under deny = %v, want ErrProjectNotFound", err)
	}
}

// TestServiceConditionalVerdictFailsClosed: a conditional verdict the
// site does not resolve (T0105 has no condition resolvers yet) refuses
// the action — the safe default for an unresolved condition is denial,
// never allow.
func TestServiceConditionalVerdictFailsClosed(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	cond := &stubEngine{decision: authz.Decision{Verdict: authz.VerdictConditional}}
	svc := NewService(store, store.gate, cond)
	if _, _, err := svc.Create(ctx, testUser("alice"), CreateProjectInput{
		Slug: "p", Name: "P", Purpose: "x", Visibility: domain.VisibilityPublic,
	}); !errors.Is(err, ErrForbidden) {
		t.Errorf("Create under conditional = %v, want ErrForbidden (fail closed)", err)
	}
}

// TestServiceEngineFailureFailsClosed: an engine error means state is
// unknowable — the action is refused with ErrStore (503), never guessed.
func TestServiceEngineFailureFailsClosed(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	broken := &stubEngine{err: errors.New("engine down")}
	svc := NewService(store, store.gate, broken)
	if _, _, err := svc.Create(ctx, testUser("alice"), CreateProjectInput{
		Slug: "p", Name: "P", Purpose: "x", Visibility: domain.VisibilityPublic,
	}); !errors.Is(err, ErrStore) {
		t.Errorf("Create under engine failure = %v, want wrapped ErrStore", err)
	}
}

// TestServiceWithoutEngineFailsClosed: a service wired without a policy
// engine refuses everything — a missing engine is never "everyone
// allowed" (default deny, docs/12).
func TestServiceWithoutEngineFailsClosed(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	svc := NewService(store, store.gate, nil)
	if _, _, err := svc.Create(ctx, testUser("alice"), CreateProjectInput{
		Slug: "p", Name: "P", Purpose: "x", Visibility: domain.VisibilityPublic,
	}); !errors.Is(err, ErrStore) {
		t.Errorf("Create without engine = %v, want wrapped ErrStore (fail closed)", err)
	}
	if _, err := svc.List(ctx, testUser("alice")); err != nil {
		t.Errorf("List without engine = %v (list needs no project-scoped check: the query is the filter)", err)
	}
}
