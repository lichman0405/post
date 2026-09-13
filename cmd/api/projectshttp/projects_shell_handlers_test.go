package projectshttp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/memstore"
)

// The membership endpoint's wire mapping, tested at the handler level
// through the REAL guard (in-memory adapters: a real signup issues the
// session the guard resolves). ErrMemberNotFound must answer 404 with the
// stable PROJECT_MEMBERSHIP_NOT_FOUND code (the shape the browser shell's
// Settings gate consumes), and an unreadable project must answer the
// existence-hiding PROJECT_NOT_FOUND. The permissive engine stands in for
// the visibility-aware read so the "readable project, no membership"
// branch is reachable without PostgreSQL (the full read policy over the
// real database is the integration suite's ground; T0106 extends reads to
// public projects).

// stubProjectStore implements just enough of projects.ProjectStore. The
// membership answer is keyed dynamically on the requested user id — the
// real signup decides the actor's id, so the fixture cannot know it in
// advance; the assertions below check the round-tripped id instead.
type stubProjectStore struct {
	project domain.Project
	// member, when true, answers owner for every caller; when false the
	// store answers ErrMemberNotFound.
	member bool
}

func (s *stubProjectStore) CreateProject(context.Context, domain.Project, string) (domain.Project, domain.ProjectMembership, error) {
	return domain.Project{}, domain.ProjectMembership{}, projects.ErrStore
}

func (s *stubProjectStore) GetProject(_ context.Context, projectID string) (domain.Project, error) {
	if s.project.ID == projectID {
		return s.project, nil
	}
	return domain.Project{}, projects.ErrProjectNotFound
}

func (s *stubProjectStore) GetMembership(_ context.Context, projectID, userID string) (domain.ProjectMembership, error) {
	if s.member && s.project.ID == projectID {
		return domain.ProjectMembership{
			ProjectID: projectID,
			UserID:    userID,
			Role:      domain.ProjectRoleOwner,
			CreatedAt: time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC),
		}, nil
	}
	return domain.ProjectMembership{}, projects.ErrMemberNotFound
}

func (s *stubProjectStore) ListProjectsForUser(context.Context, string) ([]domain.Project, error) {
	return nil, projects.ErrStore
}

// T0106 added ListPublicProjects to the store interface (visibility-aware
// reads); the stub answers like its siblings so the shell tests keep testing
// the shell rather than the store.
func (s *stubProjectStore) ListPublicProjects(context.Context) ([]domain.Project, error) {
	return nil, projects.ErrStore
}

func (s *stubProjectStore) GetProgram(context.Context, string) (domain.Program, error) {
	return domain.Program{}, projects.ErrProgramNotFound
}

// stubEngine allows every request, so the service's own authorization is
// not what this test exercises (the matrix is covered elsewhere); it is
// the read-visibility stand-in for a project a caller may read.
type stubEngine struct{}

func (stubEngine) Authorize(context.Context, authz.Request) (authz.Decision, error) {
	return authz.Decision{Verdict: authz.VerdictAllow}, nil
}

// stubOrgGate satisfies projects.OrgGate (unused by these routes).
type stubOrgGate struct{}

func (stubOrgGate) GetOrganization(context.Context, string) (domain.Organization, error) {
	return domain.Organization{}, projects.ErrOrgNotFound
}

func (stubOrgGate) GetMembership(context.Context, string, string) (domain.OrganizationMembership, error) {
	return domain.OrganizationMembership{}, projects.ErrMemberNotFound
}

// newShellTestServer composes the auth surface (real signup/session flow,
// in-memory) with the membership route, exactly as the integration suite
// composes the production tree — and returns a browser holding a real
// session cookie, plus the signed-up user's id.
func newShellTestServer(t *testing.T, store projects.ProjectStore) (*httptest.Server, *http.Client, string) {
	t.Helper()
	authAPI := authhttp.New(authhttp.Deps{
		Users:      memstore.NewUsers(),
		Sessions:   memstore.NewSessions(),
		Limiter:    memstore.NewLimiter(),
		OIDCClient: nil,
		Cfg: authn.Config{
			WebOrigin:          "http://web.test",
			SessionTTL:         time.Hour,
			LoginLimitPerEmail: 1000,
			LoginLimitPerIP:    10000,
			LoginWindow:        time.Minute,
			SignupLimitPerIP:   10000,
		},
		Secure: false,
	})
	projectAPI := New(Deps{
		Store: store,
		Orgs:  &stubOrgGate{},
		Authz: stubEngine{},
	})
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	mux.Handle("/api/v1/projects/", projectAPI.Routes())
	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)

	// Real signup: the session lands in the in-memory store and the
	// cookie jar carries it on every membership read.
	jar, _ := cookiejar.New(nil)
	authed := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	body := `{"email":"shell-handler@example.com","password":"long-enough-password-1","handle":"shell-handler","display_name":"Shell Handler"}`
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/auth/signup", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := authed.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("signup = %d: %s", resp.StatusCode, b)
	}
	var payload struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return ts, authed, payload.User.ID
}

func TestHandleMembershipWireMapping(t *testing.T) {
	const projectID = "11111111-2222-4333-8444-555555555555"
	project := domain.Project{
		ID: projectID, Slug: "lab", Name: "Lab",
		Visibility: domain.VisibilityPublic,
	}

	t.Run("member role round-trips", func(t *testing.T) {
		ts, authed, userID := newShellTestServer(t, &stubProjectStore{project: project, member: true})
		resp, err := authed.Get(ts.URL + "/api/v1/projects/" + projectID + "/membership")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var payload struct {
			ProjectID string `json:"project_id"`
			UserID    string `json:"user_id"`
			Role      string `json:"role"`
			CreatedAt string `json:"created_at"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.ProjectID != projectID || payload.UserID != userID || payload.Role != "owner" {
			t.Errorf("payload = %+v, want owner membership of %s for %s", payload, projectID, userID)
		}
		if payload.CreatedAt == "" {
			t.Error("created_at must be present")
		}
	})

	t.Run("no role answers the stable 404 code", func(t *testing.T) {
		ts, authed, _ := newShellTestServer(t, &stubProjectStore{project: project, member: false})
		resp, err := authed.Get(ts.URL + "/api/v1/projects/" + projectID + "/membership")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		var envelope struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Code != projects.CodeProjectMembershipNotFound {
			t.Errorf("code = %q, want %q", envelope.Code, projects.CodeProjectMembershipNotFound)
		}
	})

	t.Run("unreadable project answers existence hiding", func(t *testing.T) {
		ts, authed, _ := newShellTestServer(t, &stubProjectStore{project: project, member: false})
		resp, err := authed.Get(ts.URL + "/api/v1/projects/99999999-8888-4777-8666-555555555555/membership")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		var envelope struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Code != projects.CodeProjectNotFound {
			t.Errorf("code = %q, want %q", envelope.Code, projects.CodeProjectNotFound)
		}
	})
}
