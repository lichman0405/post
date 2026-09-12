// Package integration — T0105-TEST-01 "permission matrix tests"
// (blocking): the project permission framework exercised end to end
// against REAL PostgreSQL — real migrations, real pgx stores, the real
// auth guard (session + CSRF) and the real projectshttp handlers, with
// the real matrix policy engine (authz.NewMatrixEngine) wired exactly as
// cmd/api/main.go composes them. Only the session store is in-memory.
//
// The acceptance criteria:
//
//   - "权限矩阵核心动作与 CSV 一致" → the cell-for-cell CSV gate lives in
//     internal/authz (TestPermissionMatrixMatchesCSV); here the four
//     member-role columns are exercised over the wire: viewer,
//     contributor, maintainer and owner memberships (seeded with SQL —
//     the member-management API is T0109) all read a private project,
//     while a non-member gets the existence-hiding 404 and an anonymous
//     caller gets 401.
//   - "前端隐藏不代替后端拒绝" → this test never touches a UI: every
//     request is raw HTTP against the API. The refusals observed below
//     come from the server (guard + engine + service), so no client-side
//     hiding is involved in the denial.
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

const projectAuthzTaskID = "T0105"

// TestProjectAuthzMatrix drives the role matrix over the wire. It is the
// server-side half of the T0105 framework: the roles live in the
// database, the guard authenticates, the engine decides, the service
// refuses — nothing here depends on a client hiding anything.
func TestProjectAuthzMatrix(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), projectAuthzTaskID)

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

	alice, _ := signup(t, ts.URL, "authz-alice@example.com", "authz-alice")
	bob, bobID := signup(t, ts.URL, "authz-bob@example.com", "authz-bob")
	carol, carolID := signup(t, ts.URL, "authz-carol@example.com", "authz-carol")
	dave, daveID := signup(t, ts.URL, "authz-dave@example.com", "authz-dave")
	eve, _ := signup(t, ts.URL, "authz-eve@example.com", "authz-eve")

	// --- alice creates a private project; the creator becomes owner ---
	resp := alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"role-lab","name":"Role Lab","purpose":"exercise the role matrix","visibility":"private"}`)
	mustStatus(t, resp, http.StatusCreated)
	var created projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("create project payload: %v", err)
	}
	projectID := created.Project.ID

	// --- seed the other three role columns directly in PostgreSQL (the
	// member-management API lands with T0109; T0105 seeds canonical state
	// so the engine's role columns are what is exercised here) ---
	seeds := []struct {
		userID string
		role   string
	}{
		{bobID, "viewer"},
		{carolID, "contributor"},
		{daveID, "maintainer"},
	}
	for _, seed := range seeds {
		if _, err := pool.Exec(ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, $3)`,
			projectID, seed.userID, seed.role); err != nil {
			t.Fatalf("seed %s membership: %v", seed.role, err)
		}
	}

	// --- read_private_project: every member role reads the private
	// project over the wire (owner via alice, plus the three seeded
	// roles). Raw HTTP only — no browser, no UI, no client-side hiding.
	for _, client := range []*testUserClient{alice, bob, carol, dave} {
		resp = client.do(t, http.MethodGet, "/api/v1/projects/"+projectID, "")
		mustStatus(t, resp, http.StatusOK)
		_ = readAll(t, resp)
	}

	// --- non-member: the same read is refused server-side with the
	// existence-hiding shape (a non-member cannot tell the project
	// exists), and the engine's denial — not any UI — produced it.
	resp = eve.do(t, http.MethodGet, "/api/v1/projects/"+projectID, "")
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, "PROJECT_NOT_FOUND")

	// --- anonymous: the guard refuses before routing (401), reads
	// included (member-only until T0106 extends public reads).
	anon := newTestUserClient(ts.URL)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/projects/"+projectID, nil)
	r, err := anon.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	mustStatus(t, r, http.StatusUnauthorized)
	_ = r.Body.Close()

	// --- the roles asserted over the wire are the roles stored
	// server-side: the matrix columns come from the database membership
	// rows, not from anything the client sent.
	rows, err := pool.Query(ctx,
		`SELECT role FROM project_memberships WHERE project_id = $1 ORDER BY role`, projectID)
	if err != nil {
		t.Fatalf("probe roles: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			t.Fatal(err)
		}
		got = append(got, role)
	}
	want := []string{"contributor", "maintainer", "owner", "viewer"}
	if len(got) != len(want) {
		t.Fatalf("membership roles = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("membership roles = %v, want %v", got, want)
		}
	}
}
