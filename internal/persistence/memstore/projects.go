package memstore

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/lichman0405/post/internal/application/orgs"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
)

// Projects is an in-memory projects.ProjectStore: one project space with
// organizations, programs and project memberships attached, mirroring how
// production keeps them side by side in PostgreSQL. It reproduces the
// production adapter's invariants (org gate, program belonging, per-scope
// slug uniqueness, creator's owner membership) exactly, so the e2e suites
// can exercise the real service policy without a database — the
// production adapter's SQL is covered by tests/integration against real
// PostgreSQL. The store is safe for concurrent use.
//
// NOT for production project storage: POST canonical truth is PostgreSQL
// (CLAUDE.md §8); the production adapter is persistence.ProjectStore.
//
// The organization gate is a separate type (Orgs) over the same shared
// state: the two ports both spell their membership lookup "GetMembership"
// but with different shapes, so no single Go type can implement both.
type Projects struct {
	mu       sync.Mutex
	orgs     map[string]domain.Organization
	orgMembs map[[2]string]domain.OrganizationMembership
	programs map[string]domain.Program
	projects map[string]domain.Project
	members  map[[2]string]domain.ProjectMembership
	nextID   int
}

// NewProjects builds an empty in-memory project store.
func NewProjects() *Projects {
	return &Projects{
		orgs:     map[string]domain.Organization{},
		orgMembs: map[[2]string]domain.OrganizationMembership{},
		programs: map[string]domain.Program{},
		projects: map[string]domain.Project{},
		members:  map[[2]string]domain.ProjectMembership{},
	}
}

// Gate returns the organization-gate half of this space (projects.OrgGate)
// — the gate the project service consults and the state CreateProject
// re-checks are one and the same.
func (s *Projects) Gate() *Orgs { return &Orgs{p: s} }

// SeedOrg registers an active organization with an active owner
// membership (test/dev bootstrap) — the same shape the production org
// store would have returned after T0103's create-organization flow.
func (s *Projects) SeedOrg(orgID, ownerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orgs[orgID] = domain.Organization{ID: orgID, Slug: "org-" + orgID, Name: "Org " + orgID}
	s.orgMembs[[2]string{orgID, ownerID}] = domain.OrganizationMembership{
		OrganizationID: orgID, UserID: ownerID, Role: domain.OrgRoleOwner, Verified: true,
	}
}

// freshID returns a deterministic id shaped like a uuid v4 text form (the
// production uuid column's shape), so tests stay reproducible.
func (s *Projects) freshID() string {
	s.nextID++
	return freshUUID(s.nextID)
}

// CreateProject implements projects.ProjectStore: inserts the project and
// the creator's owner membership atomically (single lock), re-checking
// the organization gate inside — a create racing a concurrent
// deactivation or affiliation end cannot land a project in a deactivated
// organization or for a departed member, exactly like the production
// transaction.
func (s *Projects) CreateProject(_ context.Context, p domain.Project, creatorUserID string) (domain.Project, domain.ProjectMembership, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.OrganizationID != nil {
		org, ok := s.orgs[*p.OrganizationID]
		if !ok {
			return domain.Project{}, domain.ProjectMembership{}, projects.ErrOrgNotFound
		}
		if !org.Active() {
			return domain.Project{}, domain.ProjectMembership{}, projects.ErrOrgDeactivated
		}
		m, ok := s.orgMembs[[2]string{*p.OrganizationID, creatorUserID}]
		if !ok || !m.Active() {
			return domain.Project{}, domain.ProjectMembership{}, projects.ErrForbidden
		}
	}
	if p.ProgramID != nil {
		prog, ok := s.programs[*p.ProgramID]
		if !ok {
			return domain.Project{}, domain.ProjectMembership{}, projects.ErrProgramNotFound
		}
		if prog.OrganizationID == nil || p.OrganizationID == nil || *prog.OrganizationID != *p.OrganizationID {
			return domain.Project{}, domain.ProjectMembership{}, projects.ErrProgramOrgMismatch
		}
	}
	for _, existing := range s.projects {
		if existing.Slug != p.Slug {
			continue
		}
		if (existing.OrganizationID == nil && p.OrganizationID == nil) ||
			(existing.OrganizationID != nil && p.OrganizationID != nil && *existing.OrganizationID == *p.OrganizationID) {
			return domain.Project{}, domain.ProjectMembership{}, projects.ErrSlugTaken
		}
	}
	p.ID = s.freshID()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	s.projects[p.ID] = p
	m := domain.ProjectMembership{ProjectID: p.ID, UserID: creatorUserID, Role: domain.ProjectRoleOwner, CreatedAt: p.CreatedAt}
	s.members[[2]string{p.ID, creatorUserID}] = m
	return p, m, nil
}

// GetProject implements projects.ProjectStore.
func (s *Projects) GetProject(_ context.Context, projectID string) (domain.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[projectID]
	if !ok {
		return domain.Project{}, projects.ErrProjectNotFound
	}
	return p, nil
}

// GetMembership implements projects.ProjectStore.
func (s *Projects) GetMembership(_ context.Context, projectID, userID string) (domain.ProjectMembership, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.members[[2]string{projectID, userID}]
	if !ok {
		return domain.ProjectMembership{}, projects.ErrMemberNotFound
	}
	return m, nil
}

// ListProjectsForUser implements projects.ProjectStore.
func (s *Projects) ListProjectsForUser(_ context.Context, userID string) ([]domain.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Project
	for _, m := range s.members {
		if m.UserID == userID {
			out = append(out, s.projects[m.ProjectID])
		}
	}
	sortNewestFirst(out)
	return out, nil
}

// ListPublicProjects implements projects.ProjectStore. The visibility
// predicate is the read policy (T0106): private projects are excluded
// here, so no caller identity participates and nothing can leak into the
// result set.
func (s *Projects) ListPublicProjects(_ context.Context) ([]domain.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Project
	for _, p := range s.projects {
		if p.Visibility == domain.VisibilityPublic {
			out = append(out, p)
		}
	}
	sortNewestFirst(out)
	return out, nil
}

// GetProgram implements projects.ProjectStore.
func (s *Projects) GetProgram(_ context.Context, programID string) (domain.Program, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.programs[programID]
	if !ok {
		return domain.Program{}, projects.ErrProgramNotFound
	}
	return p, nil
}

// Orgs is the organization-gate half of a Projects space (projects.OrgGate),
// sharing the store's state: the gate the project service consults and the
// state CreateProject re-checks are one and the same. It answers with the
// orgs application's sentinels exactly like the production adapter
// (persistence.OrgStore).
type Orgs struct {
	p *Projects
}

// GetOrganization implements projects.OrgGate.
func (g *Orgs) GetOrganization(_ context.Context, orgID string) (domain.Organization, error) {
	g.p.mu.Lock()
	defer g.p.mu.Unlock()
	o, ok := g.p.orgs[orgID]
	if !ok {
		return domain.Organization{}, orgs.ErrOrgNotFound
	}
	return o, nil
}

// GetMembership implements projects.OrgGate.
func (g *Orgs) GetMembership(_ context.Context, orgID, userID string) (domain.OrganizationMembership, error) {
	g.p.mu.Lock()
	defer g.p.mu.Unlock()
	m, ok := g.p.orgMembs[[2]string{orgID, userID}]
	if !ok {
		return domain.OrganizationMembership{}, orgs.ErrMemberNotFound
	}
	return m, nil
}

// sortNewestFirst orders projects by creation time, newest first, with
// the id as the deterministic tie-breaker (the same order the production
// queries produce).
func sortNewestFirst(projects []domain.Project) {
	sort.Slice(projects, func(i, j int) bool {
		a, b := projects[i], projects[j]
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c > 0
		}
		return a.ID < b.ID
	})
}

var (
	_ projects.ProjectStore = (*Projects)(nil)
	_ projects.OrgGate      = (*Orgs)(nil)
)
