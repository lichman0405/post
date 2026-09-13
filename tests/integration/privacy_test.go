// Package integration — T0106-TEST-01 "privacy negative e2e" (blocking,
// real-PostgreSQL half): the public/private read isolation exercised end
// to end against REAL PostgreSQL — real migrations, real pgx stores, the
// real auth guard (session + CSRF) and the real projectshttp handlers,
// exactly as cmd/api/main.go composes them. Only the session store is
// in-memory. The in-memory-store half of the same gate lives in
// tests/e2e (TestE2EPrivacyNegative); here the point is the production
// SQL path — the visibility predicate of ListPublicProjects is the read
// policy, and it is asserted at the store level as well as over the wire.
//
// The task requirements each map to a phase of the test:
//
//   - "匿名可读 public project shell"           → anonymous GET of a public
//     project answers 200 with the full shell payload.
//   - "private project 对未授权返回不泄漏式
//     404/403 policy"                           → anonymous and
//     authenticated-non-member GETs of a private project answer the same
//     existence-hiding 404 envelope an unknown id produces, and no
//     project metadata appears anywhere in the response body.
//   - "列表过滤"                                 → the anonymous list contains
//     public projects only; the private projects' ids/slugs/names never
//     appear anywhere in the response body (raw-body check, not just a
//     decoded-struct check).
package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

const privacyTaskID = "T0106"

// TestPrivacyReadIsolation drives the T0106 privacy negatives over the
// wire against real PostgreSQL, and asserts the SQL visibility filter at
// the store level.
func TestPrivacyReadIsolation(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), privacyTaskID)

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
	projectStore := persistence.NewProjectStore(pool)
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: projectStore,
		Orgs:  persistence.NewOrgStore(pool),
		Authz: authz.NewMatrixEngine(),
	})
	apiMux := http.NewServeMux()
	apiMux.Handle("/api/v1/auth/", authAPI.Routes())
	apiMux.Handle("/api/v1/projects", projectAPI.Routes())
	apiMux.Handle("/api/v1/projects/", projectAPI.Routes())
	ts := httptest.NewServer(authAPI.Guard(apiMux))
	defer ts.Close()

	alice, _ := signup(t, ts.URL, "privacy-alice@example.com", "privacy-alice")
	bob, _ := signup(t, ts.URL, "privacy-bob@example.com", "privacy-bob")
	anon := newTestUserClient(ts.URL)

	// Alice: one public, one private project (personal — the visibility
	// column is the same isolation boundary for org projects).
	resp := alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"open-lab","name":"Open MOF Lab","purpose":"public MOF screening","visibility":"public"}`)
	mustStatus(t, resp, http.StatusCreated)
	var pub projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&pub); err != nil {
		t.Fatalf("public project payload: %v", err)
	}
	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"secret-lab","name":"Secret Zeolite Lab","purpose":"unpublished zeolite work","visibility":"private"}`)
	mustStatus(t, resp, http.StatusCreated)
	var priv projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&priv); err != nil {
		t.Fatalf("private project payload: %v", err)
	}
	// Bob: his own private project.
	resp = bob.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"bob-secret","name":"Bob Hidden Lab","purpose":"bob private research","visibility":"private"}`)
	mustStatus(t, resp, http.StatusCreated)
	var bobPriv projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&bobPriv); err != nil {
		t.Fatalf("bob private payload: %v", err)
	}

	// --- anonymous list: public only (requirement: 列表过滤) ------------
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/projects", nil)
	resp, err := anon.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	mustStatus(t, resp, http.StatusOK)
	raw := readAll(t, resp)
	var list projectListResponse
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatalf("anonymous list payload: %v", err)
	}
	if len(list.Projects) != 1 || list.Projects[0].ID != pub.Project.ID {
		t.Fatalf("anonymous list = %+v, want exactly the public project", list.Projects)
	}
	// Structural check: no private project identifier or free-text value
	// may appear anywhere in the raw anonymous list body.
	for _, leak := range []string{
		"secret-lab", "Secret Zeolite Lab", "unpublished zeolite work",
		"bob-secret", "Bob Hidden Lab", "bob private research",
		priv.Project.ID, bobPriv.Project.ID,
	} {
		if strings.Contains(raw, leak) {
			t.Errorf("anonymous list body leaks %q: %s", leak, raw)
		}
	}

	// --- anonymous direct read: public shell readable (requirement:
	// 匿名可读 public project shell) ---
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/v1/projects/"+pub.Project.ID, nil)
	resp, err = anon.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	mustStatus(t, resp, http.StatusOK)
	var anonPub projectResponse
	if err := json.Unmarshal([]byte(readAll(t, resp)), &anonPub.Project); err != nil {
		t.Fatalf("anonymous public read payload: %v", err)
	}
	if anonPub.Project.Visibility != "public" || anonPub.Project.Name != "Open MOF Lab" {
		t.Errorf("anonymous public read = %+v", anonPub.Project)
	}

	// --- anonymous id guessing: no metadata leak (requirement: private
	// 对未授权不泄漏式 404/403) — a guessed private id and a guessed
	// unknown id answer the identical envelope, and neither body carries
	// any project metadata.
	for _, id := range []string{priv.Project.ID, bobPriv.Project.ID, "00000000-0000-4000-8000-000000000000"} {
		req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/v1/projects/"+id, nil)
		resp, err = anon.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		mustStatus(t, resp, http.StatusNotFound)
		raw := readAll(t, resp)
		var env errorEnvelope
		if err := json.Unmarshal([]byte(raw), &env); err != nil || env.Code != "PROJECT_NOT_FOUND" {
			t.Errorf("anonymous 404 for %s envelope = %s (err %v), want PROJECT_NOT_FOUND", id, raw, err)
		}
		if strings.Contains(raw, "secret-lab") || strings.Contains(raw, "bob-secret") ||
			strings.Contains(raw, "Zeolite") || strings.Contains(raw, "Hidden") {
			t.Errorf("anonymous 404 for %s leaks project metadata: %s", id, raw)
		}
	}

	// --- authenticated non-member: private stays hidden -----------------
	resp = bob.do(t, http.MethodGet, "/api/v1/projects/"+priv.Project.ID, "")
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, "PROJECT_NOT_FOUND")
	// Bob's list: the public project plus his own — alice's private
	// project never appears.
	resp = bob.do(t, http.MethodGet, "/api/v1/projects", "")
	mustStatus(t, resp, http.StatusOK)
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("bob list payload: %v", err)
	}
	bobIDs := map[string]bool{}
	for _, p := range list.Projects {
		bobIDs[p.ID] = true
		if p.ID == priv.Project.ID {
			t.Errorf("bob's list leaked alice's private project: %+v", p)
		}
	}
	if !bobIDs[pub.Project.ID] || !bobIDs[bobPriv.Project.ID] {
		t.Errorf("bob's list = %+v, want public open-lab + own bob-secret", list.Projects)
	}

	// --- the owner still reads her private project ----------------------
	resp = alice.do(t, http.MethodGet, "/api/v1/projects/"+priv.Project.ID, "")
	mustStatus(t, resp, http.StatusOK)
	_ = readAll(t, resp)

	// --- the SQL filter itself: the store's public list contains exactly
	// the public rows even though three rows exist, two of them private.
	// This is the production query the wire behavior above depends on.
	publics, err := projectStore.ListPublicProjects(ctx)
	if err != nil {
		t.Fatalf("ListPublicProjects: %v", err)
	}
	if len(publics) != 1 || publics[0].ID != pub.Project.ID {
		t.Errorf("store public list = %+v, want exactly the public project", publics)
	}
	var privateCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM projects WHERE visibility = 'private'`).Scan(&privateCount); err != nil {
		t.Fatalf("probe private count: %v", err)
	}
	if privateCount != 2 {
		t.Errorf("private rows in database = %d, want 2 (the filter excluded them, not the data)", privateCount)
	}

	// --- the visibility column in the database is the boundary the wire
	// behavior encodes: a private row answers read_private_project, which
	// the anonymous class denies (asserted over the wire above) ---
	var dbVisibility string
	if err := pool.QueryRow(ctx,
		`SELECT visibility FROM projects WHERE id = $1`, priv.Project.ID).Scan(&dbVisibility); err != nil {
		t.Fatalf("probe visibility: %v", err)
	}
	if dbVisibility != string(domain.VisibilityPrivate) {
		t.Errorf("database visibility = %q, want private", dbVisibility)
	}
}
