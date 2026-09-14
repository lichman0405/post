package validationhttp

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
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/memstore"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// The :validate route's visibility gate, tested at the handler level
// through the REAL guard and the REAL T0106 matrix engine (a real signup
// issues the session the guard resolves; the projects service evaluates
// the actual read policy). A report about a branch is exactly as visible
// as its project: a private project answers the existence-hiding 404 for
// an authenticated non-member, and a member (or any caller of a public
// project) gets the report.

// visProjectStore implements projects.ProjectStore for the visibility
// tests. The membership answer is keyed dynamically on the requested user
// id — the real signup decides the actor's id, so the fixture cannot know
// it in advance; member controls whether every caller is answered as an
// owner.
type visProjectStore struct {
	project domain.Project
	member  bool
}

func (s *visProjectStore) CreateProject(context.Context, domain.Project, string) (domain.Project, domain.ProjectMembership, error) {
	return domain.Project{}, domain.ProjectMembership{}, projects.ErrStore
}

func (s *visProjectStore) GetProject(_ context.Context, projectID string) (domain.Project, error) {
	if s.project.ID == projectID {
		return s.project, nil
	}
	return domain.Project{}, projects.ErrProjectNotFound
}

func (s *visProjectStore) GetMembership(_ context.Context, projectID, userID string) (domain.ProjectMembership, error) {
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

func (s *visProjectStore) ListProjectMembers(context.Context, string) ([]domain.ProjectMember, error) {
	return nil, nil
}

func (s *visProjectStore) UpdateMembershipRole(context.Context, string, string, domain.ProjectRole, domain.AuditEntry) (domain.ProjectMembership, error) {
	return domain.ProjectMembership{}, projects.ErrStore
}

func (s *visProjectStore) UpdateProjectSettings(context.Context, string, *string, *string, domain.AuditEntry) (domain.Project, error) {
	return domain.Project{}, projects.ErrStore
}

func (s *visProjectStore) ListProjectsForUser(context.Context, string) ([]domain.Project, error) {
	return nil, nil
}

func (s *visProjectStore) ListPublicProjects(context.Context) ([]domain.Project, error) {
	return nil, nil
}

func (s *visProjectStore) GetProgram(context.Context, string) (domain.Program, error) {
	return domain.Program{}, projects.ErrProgramNotFound
}

// visOrgGate satisfies projects.OrgGate (unused by the validate route).
type visOrgGate struct{}

func (visOrgGate) GetOrganization(context.Context, string) (domain.Organization, error) {
	return domain.Organization{}, projects.ErrOrgNotFound
}

func (visOrgGate) GetMembership(context.Context, string, string) (domain.OrganizationMembership, error) {
	return domain.OrganizationMembership{}, projects.ErrMemberNotFound
}

// newVisibilityTestServer composes the auth surface (real signup/session
// flow, in-memory), the projects surface (real service, real matrix
// engine, canned store) and the validation surface — exactly the
// production composition — and returns a browser holding a real session
// cookie, the signed-up user's id and the CSRF token the write must echo.
func newVisibilityTestServer(t *testing.T, store projects.ProjectStore, validator BranchValidator) (*httptest.Server, *http.Client, string, string) {
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
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: store,
		Orgs:  visOrgGate{},
		Authz: authz.NewMatrixEngine(),
	})
	validationAPI := New(Deps{Validator: validator, Projects: projectAPI.Service()})
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	mux.Handle("/api/v1/projects/", projectAPI.Routes())
	validationAPI.Register(mux)
	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)

	// Real signup: the session lands in the in-memory store and the
	// cookie jar carries it on every request.
	jar, _ := cookiejar.New(nil)
	authed := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	body := `{"email":"validate-visibility@example.com","password":"long-enough-password-1","handle":"validate-visibility","display_name":"Validate Visibility"}`
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
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode signup: %v", err)
	}
	return ts, authed, payload.User.ID, payload.CSRFToken
}

// validateWrite performs the guarded POST :validate with the CSRF token
// attached, the way the browser client sends state changes.
func validateWrite(t *testing.T, client *http.Client, url, csrf string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"gate":"pr"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestValidateRouteVisibility(t *testing.T) {
	const projectID = "11111111-2222-4333-8444-555555555555"
	const branchID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	url := "/api/v1/projects/" + projectID + "/branches/" + branchID + ":validate"

	t.Run("private project hides the report from a non-member", func(t *testing.T) {
		store := &visProjectStore{project: domain.Project{
			ID: projectID, Slug: "private-lab", Name: "Private Lab",
			Visibility: domain.VisibilityPrivate,
		}, member: false}
		validator := &fakeValidator{report: rsgvalidation.Report{Gate: rsgvalidation.GatePR, Verdict: rsgvalidation.VerdictPass}}
		ts, authed, _, csrf := newVisibilityTestServer(t, store, validator)

		resp := validateWrite(t, authed, ts.URL+url, csrf)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (existence hiding for the non-member)", resp.StatusCode)
		}
		var envelope struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		if envelope.Code != projects.CodeProjectNotFound {
			t.Errorf("code = %q, want %q (the same non-leaking shape as every project read)", envelope.Code, projects.CodeProjectNotFound)
		}
		if validator.gotProjectID != "" {
			t.Error("the validator ran behind a denied project read — the report must never be assembled for a caller who may not read the project")
		}
	})

	t.Run("private project serves the report to a member", func(t *testing.T) {
		store := &visProjectStore{project: domain.Project{
			ID: projectID, Slug: "private-lab", Name: "Private Lab",
			Visibility: domain.VisibilityPrivate,
		}, member: true}
		validator := &fakeValidator{report: rsgvalidation.Report{Gate: rsgvalidation.GatePR, Verdict: rsgvalidation.VerdictPass}}
		ts, authed, _, csrf := newVisibilityTestServer(t, store, validator)

		resp := validateWrite(t, authed, ts.URL+url, csrf)
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, b)
		}
		var report rsgvalidation.Report
		if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
			t.Fatalf("decode report: %v", err)
		}
		if report.Gate != rsgvalidation.GatePR {
			t.Errorf("report gate = %s, want pr", report.Gate)
		}
		if validator.gotProjectID != projectID || validator.gotBranchID != branchID {
			t.Errorf("validator called for (%q, %q), want the requested project and branch", validator.gotProjectID, validator.gotBranchID)
		}
	})

	t.Run("public project serves the report to a non-member", func(t *testing.T) {
		store := &visProjectStore{project: domain.Project{
			ID: projectID, Slug: "public-lab", Name: "Public Lab",
			Visibility: domain.VisibilityPublic,
		}, member: false}
		validator := &fakeValidator{report: rsgvalidation.Report{Gate: rsgvalidation.GatePR, Verdict: rsgvalidation.VerdictPass}}
		ts, authed, _, csrf := newVisibilityTestServer(t, store, validator)

		resp := validateWrite(t, authed, ts.URL+url, csrf)
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 200 (read_public_project allows every class): %s", resp.StatusCode, b)
		}
	})

	t.Run("anonymous write is answered by the guard", func(t *testing.T) {
		store := &visProjectStore{project: domain.Project{
			ID: projectID, Slug: "public-lab", Name: "Public Lab",
			Visibility: domain.VisibilityPublic,
		}}
		validator := &fakeValidator{}
		ts, _, _, _ := newVisibilityTestServer(t, store, validator)

		// No session cookie, no CSRF token: the guard answers 401 before
		// the route ever runs.
		resp, err := http.Post(ts.URL+url, "application/json", strings.NewReader(`{"gate":"pr"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})
}
