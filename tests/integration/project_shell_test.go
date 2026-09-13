// Package integration — T0108 shell membership read exercised end to end
// against REAL PostgreSQL: real migrations, real pgx stores, the real
// auth guard and the real projectshttp handlers. The membership endpoint
// is the API half of the project shell: it tells the caller their own
// role (the data the Settings tab gate is driven by) and it must never
// disclose a membership — or the project's existence — to a caller who
// may not read the project.
package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/orgshttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

const projectShellTaskID = "T0108"

// projectMembershipResponse is the wire shape of
// GET /api/v1/projects/{projectId}/membership.
type projectMembershipResponse struct {
	ProjectID string `json:"project_id"`
	UserID    string `json:"user_id"`
	Role      string `json:"role"`
	CreatedAt string `json:"created_at"`
}

// TestProjectShellMembership drives the shell's own-membership read over
// the wire: the caller's role round-trips, a seeded member reads their
// own role, a non-member gets the existence-hiding 404 (never a distinct
// "you are not a member" that would disclose the project), an anonymous
// caller gets 401, and an unknown project answers the same 404 shape.
func TestProjectShellMembership(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), projectShellTaskID)

	// --- composition (identical to cmd/api/main.go) ---
	sessions := memstore.NewSessions()
	limiter := memstore.NewLimiter()
	cfg := authn.Config{
		WebOrigin:          "http://web.test",
		SessionTTL:         time.Hour,
		LoginLimitPerEmail: 1000,
		LoginLimitPerIP:    10000,
		LoginWindow:        time.Minute,
		SignupLimitPerIP:   10000,
	}
	authAPI := authhttp.New(authhttp.Deps{
		Users:      persistence.NewCredentialStore(pool),
		Sessions:   sessions,
		Limiter:    limiter,
		OIDCClient: nil,
		Cfg:        cfg,
		Secure:     false,
	})
	orgStore := persistence.NewOrgStore(pool)
	orgAPI := orgshttp.New(orgshttp.Deps{Store: orgStore})
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: persistence.NewProjectStore(pool),
		Orgs:  orgStore,
		Authz: authz.NewMatrixEngine(),
	})
	apiMux := http.NewServeMux()
	apiMux.Handle("/api/v1/auth/", authAPI.Routes())
	apiMux.Handle("/api/v1/organizations", orgAPI.Routes())
	apiMux.Handle("/api/v1/organizations/", orgAPI.Routes())
	apiMux.Handle("/api/v1/projects", projectAPI.Routes())
	apiMux.Handle("/api/v1/projects/", projectAPI.Routes())
	ts := httptest.NewServer(authAPI.Guard(apiMux))
	defer ts.Close()

	alice, aliceID := signup(t, ts.URL, "shell-alice@example.com", "shell-alice")
	bob, bobID := signup(t, ts.URL, "shell-bob@example.com", "shell-bob")
	eve, _ := signup(t, ts.URL, "shell-eve@example.com", "shell-eve")

	// --- alice creates a private project; the creator becomes owner ---
	resp := alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"shell-lab","name":"Shell Lab","purpose":"exercise the project shell","visibility":"private"}`)
	mustStatus(t, resp, http.StatusCreated)
	var created projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("create project payload: %v", err)
	}
	projectID := created.Project.ID

	// --- the creator reads their own membership: role owner ---
	resp = alice.do(t, http.MethodGet, "/api/v1/projects/"+projectID+"/membership", "")
	mustStatus(t, resp, http.StatusOK)
	var mine projectMembershipResponse
	if err := json.NewDecoder(resp.Body).Decode(&mine); err != nil {
		t.Fatalf("membership payload: %v", err)
	}
	if mine.Role != "owner" || mine.ProjectID != projectID || mine.UserID != aliceID {
		t.Errorf("owner membership = %+v, want role owner for project %s user %s",
			mine, projectID, aliceID)
	}
	if mine.CreatedAt == "" {
		t.Error("membership payload must carry created_at")
	}

	// --- a seeded viewer reads their own role (the member-management API
	// lands with T0109; the shell gate only needs the canonical row) ---
	if _, err := pool.Exec(ctx,
		`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, $3)`,
		projectID, bobID, "viewer"); err != nil {
		t.Fatalf("seed viewer membership: %v", err)
	}
	resp = bob.do(t, http.MethodGet, "/api/v1/projects/"+projectID+"/membership", "")
	mustStatus(t, resp, http.StatusOK)
	var bobs projectMembershipResponse
	if err := json.NewDecoder(resp.Body).Decode(&bobs); err != nil {
		t.Fatalf("viewer membership payload: %v", err)
	}
	if bobs.Role != "viewer" || bobs.UserID != bobID {
		t.Errorf("viewer membership = %+v, want role viewer for %s", bobs, bobID)
	}

	// --- a non-member gets the existence-hiding shape: the same
	// PROJECT_NOT_FOUND as a project read, never a distinct "not a
	// member" — membership of an invisible project discloses nothing ---
	resp = eve.do(t, http.MethodGet, "/api/v1/projects/"+projectID+"/membership", "")
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, "PROJECT_NOT_FOUND")

	// --- an unknown project answers the same shape as a member ---
	resp = alice.do(t, http.MethodGet, "/api/v1/projects/00000000-0000-4000-8000-000000000000/membership", "")
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, "PROJECT_NOT_FOUND")

	// --- anonymous: the guard refuses reads without a session (reads are
	// member-only until T0106 extends public reads) ---
	anon := newTestUserClient(ts.URL)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/projects/"+projectID+"/membership", nil)
	r, err := anon.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	mustStatus(t, r, http.StatusUnauthorized)
	_ = r.Body.Close()
}
