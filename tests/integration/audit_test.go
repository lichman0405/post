// Package integration — T0110-TEST-01 "audit integration" (blocking): the
// audit log exercised end to end against REAL PostgreSQL — real migrations,
// real pgx stores (AuditStore + the instrumented OrgStore/ProjectStore), the
// real auth guard (session + CSRF), the real observability edge (correlation
// ids) and the real audithttp handlers, exactly as cmd/api/main.go composes
// them. Only the session store is in-memory (Redis semantics are orthogonal
// to auditing and covered by the auth e2e suite).
//
// The acceptance criteria each map to a phase of the test:
//
//   - "高风险动作有 actor/via/request id" → auth, governance, membership and
//     project-creation rows each carry the acting user, the channel (via)
//     and the request's correlation id — verified both over the wire (the
//     Activity endpoints) and with SQL against the same database.
//   - "无 update/delete audit endpoint"  → PATCH/PUT/DELETE on the activity
//     paths answer 405 at routing; the store and the handlers have no write
//     surface (the DB-level append-only triggers are T0013's append_only
//     suite).
package integration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/audithttp"
	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/orgshttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/orgs"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

const auditTaskID = "T0110"

// activityResponse is one page of the Activity feed as the wire renders it.
type activityResponse struct {
	Entries []activityEntry `json:"entries"`
	// NextCursor is the cursor for the next page, or null on the last page.
	NextCursor *string `json:"next_cursor"`
}

type activityEntry struct {
	ID               string          `json:"id"`
	ActorID          *string         `json:"actor_id"`
	ActorHandle      *string         `json:"actor_handle"`
	ActorDisplayName *string         `json:"actor_display_name"`
	Via              string          `json:"via"`
	Action           string          `json:"action"`
	TargetRef        *string         `json:"target_ref"`
	ProjectID        *string         `json:"project_id"`
	OrganizationID   *string         `json:"organization_id"`
	CorrelationID    string          `json:"correlation_id"`
	BeforeSummary    json.RawMessage `json:"before_summary"`
	AfterSummary     json.RawMessage `json:"after_summary"`
	Metadata         json.RawMessage `json:"metadata"`
	OccurredAt       time.Time       `json:"occurred_at"`
}

// auditSQLRow is one audit_log row as SQL reports it, oldest first.
type auditSQLRow struct {
	Action         string
	Via            string
	ActorID        *string
	ProjectID      *string
	OrganizationID *string
	CorrelationID  string
	BeforeSummary  *string
	AfterSummary   *string
	OccurredAt     time.Time
}

// sqlAuditRows reads the raw audit_log rows matching the scope: an empty
// scope reads the unscoped rows (auth events), otherwise the rows scoped to
// the given project or organization id, oldest first.
func sqlAuditRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, scope, scopeID string) []auditSQLRow {
	t.Helper()
	query := `SELECT action, via, actor_id::text, project_id::text, organization_id::text,
	                 correlation_id, before_summary::text, after_summary::text, occurred_at
	            FROM audit_log
	           WHERE project_id IS NULL AND organization_id IS NULL
	           ORDER BY occurred_at, id`
	var rows pgx.Rows
	var err error
	if scope != "" {
		query = `SELECT action, via, actor_id::text, project_id::text, organization_id::text,
	                 correlation_id, before_summary::text, after_summary::text, occurred_at
	            FROM audit_log
	           WHERE ($1 = 'project' AND project_id::text = $2)
	              OR ($1 = 'org' AND organization_id::text = $2)
	           ORDER BY occurred_at, id`
		rows, err = pool.Query(ctx, query, scope, scopeID)
	} else {
		rows, err = pool.Query(ctx, query)
	}
	if err != nil {
		t.Fatalf("query audit rows: %v", err)
	}
	defer rows.Close()
	var out []auditSQLRow
	for rows.Next() {
		var r auditSQLRow
		if err := rows.Scan(&r.Action, &r.Via, &r.ActorID, &r.ProjectID, &r.OrganizationID,
			&r.CorrelationID, &r.BeforeSummary, &r.AfterSummary, &r.OccurredAt); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate audit rows: %v", err)
	}
	return out
}

// mustActivity GETs one Activity page and decodes it.
func mustActivity(t *testing.T, uc *testUserClient, path string) activityResponse {
	t.Helper()
	resp := uc.do(t, http.MethodGet, path, "")
	mustStatus(t, resp, http.StatusOK)
	var page activityResponse
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("activity payload: %v", err)
	}
	return page
}

// auditCursorToken forges a client-supplied cursor token: base64url of a
// raw "timestamp|id" string — the exact shapes DecodeCursor must reject
// (B1/M2 regression: these once reached the store and panicked it).
func auditCursorToken(raw string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// csrfFromAuth extracts the freshly minted CSRF token from a login/signup
// response body (the session moved, so the browser's token must move too).
func csrfFromAuth(t *testing.T, resp *http.Response) string {
	t.Helper()
	var payload struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("auth payload: %v", err)
	}
	if payload.CSRFToken == "" {
		t.Fatal("auth response carried no csrf_token")
	}
	return payload.CSRFToken
}

// TestAuditIntegration is the T0110-TEST-01 "audit integration" gate. It
// drives the full journey over the wire against real PostgreSQL.
func TestAuditIntegration(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), auditTaskID)

	// --- composition (identical to cmd/api/main.go, observability edge
	// included — the correlation id the guard attaches to audit rows comes
	// from it) ---
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
	auditStore := persistence.NewAuditStore(pool)
	authAPI := authhttp.New(authhttp.Deps{
		Users:      persistence.NewCredentialStore(pool),
		Sessions:   sessions,
		Limiter:    limiter,
		OIDCClient: nil,
		Cfg:        cfg,
		Secure:     false,
		Audit:      auditStore,
	})
	orgStore := persistence.NewOrgStore(pool)
	orgAPI := orgshttp.New(orgshttp.Deps{Store: orgStore})
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: persistence.NewProjectStore(pool),
		Orgs:  orgStore,
		Authz: authz.NewMatrixEngine(),
	})
	auditAPI := audithttp.New(audithttp.Deps{
		Store:    auditStore,
		Projects: projectAPI.Service(),
		Orgs:     orgAPI.Service(),
	})
	apiMux := http.NewServeMux()
	authAPI.Register(apiMux)
	apiMux.Handle("/api/v1/organizations", orgAPI.Routes())
	apiMux.Handle("/api/v1/organizations/", orgAPI.Routes())
	apiMux.Handle("/api/v1/projects", projectAPI.Routes())
	apiMux.Handle("/api/v1/projects/", projectAPI.Routes())
	auditAPI.Register(apiMux)
	edge := observability.Middleware(slog.New(slog.NewTextHandler(io.Discard, nil)))(authAPI.Guard(apiMux))
	ts := httptest.NewServer(edge)
	defer ts.Close()

	alice, aliceID := signup(t, ts.URL, "audit-alice@example.com", "audit-alice")
	bob, bobID := signup(t, ts.URL, "audit-bob@example.com", "audit-bob")
	carol, _ := signup(t, ts.URL, "audit-carol@example.com", "audit-carol")

	// ---------------------------------------------------------------------
	// Phase 1 — auth events: actor/via/request id on every high-risk row.
	// ---------------------------------------------------------------------

	// Login failure for an unknown account: no actor, but the request id
	// and the via still land.
	resp := alice.do(t, http.MethodPost, "/api/v1/auth/login",
		`{"email":"nobody-here@example.com","password":"whatever-password"}`)
	unknownCorr := resp.Header.Get(observability.HeaderCorrelationID)
	mustStatus(t, resp, http.StatusUnauthorized)

	// Login failure for a known account: the actor is named.
	resp = alice.do(t, http.MethodPost, "/api/v1/auth/login",
		`{"email":"audit-alice@example.com","password":"wrong-password"}`)
	mustStatus(t, resp, http.StatusUnauthorized)

	// Login success. The login mints a fresh session, so the browser's
	// CSRF token moves with it.
	resp = alice.do(t, http.MethodPost, "/api/v1/auth/login",
		`{"email":"audit-alice@example.com","password":"long-enough-password-1"}`)
	loginCorr := resp.Header.Get(observability.HeaderCorrelationID)
	mustStatus(t, resp, http.StatusOK)
	alice.csrf = csrfFromAuth(t, resp)

	// Logout: actor comes from the guard's request identity.
	resp = alice.do(t, http.MethodPost, "/api/v1/auth/logout", "")
	logoutCorr := resp.Header.Get(observability.HeaderCorrelationID)
	mustStatus(t, resp, http.StatusNoContent)
	// Log back in for the rest of the journey.
	resp = alice.do(t, http.MethodPost, "/api/v1/auth/login",
		`{"email":"audit-alice@example.com","password":"long-enough-password-1"}`)
	mustStatus(t, resp, http.StatusOK)
	alice.csrf = csrfFromAuth(t, resp)

	authRows := sqlAuditRows(t, ctx, pool, "", "")
	// This journey produced exactly: 3 signups (alice/bob/carol), alice's
	// unknown-account failure, alice's wrong-password failure, alice's
	// login success, alice's logout, alice's re-login = 8 unscoped rows.
	if len(authRows) != 8 {
		t.Fatalf("unscoped audit rows = %d, want 8: %+v", len(authRows), authRows)
	}
	// Every row names the channel and the request (the request id is a
	// correlation id, not ""), and every row except the unknown-account
	// failure names an actor.
	for i, r := range authRows {
		if r.Via == "" {
			t.Errorf("row %d (%s): via empty", i, r.Action)
		}
		if r.CorrelationID == "" {
			t.Errorf("row %d (%s): correlation id empty — the high-risk action lacks its request id", i, r.Action)
		}
		if r.ActorID == nil && r.Action != "auth.login.failed" {
			t.Errorf("row %d (%s): actor nil", i, r.Action)
		}
	}
	// The unknown-account failure names no actor, exactly as designed.
	var unknownFail *auditSQLRow
	var wrongPassFail *auditSQLRow
	var loginSuccess *auditSQLRow
	var logout *auditSQLRow
	for i := range authRows {
		switch {
		case authRows[i].Action == "auth.login.failed" && authRows[i].ActorID == nil:
			unknownFail = &authRows[i]
		case authRows[i].Action == "auth.login.failed" && authRows[i].ActorID != nil:
			wrongPassFail = &authRows[i]
		case authRows[i].Action == "auth.login.success" && authRows[i].CorrelationID == loginCorr:
			loginSuccess = &authRows[i]
		case authRows[i].Action == "auth.logout":
			logout = &authRows[i]
		}
	}
	if unknownFail == nil || unknownFail.CorrelationID != unknownCorr {
		t.Errorf("unknown-account failure row = %+v, want correlation %q", unknownFail, unknownCorr)
	}
	if wrongPassFail == nil || wrongPassFail.ActorID == nil || *wrongPassFail.ActorID != aliceID {
		t.Errorf("known-account failure row = %+v, want actor %s", wrongPassFail, aliceID)
	}
	if loginSuccess == nil || loginSuccess.ActorID == nil || *loginSuccess.ActorID != aliceID || loginSuccess.Via != "password" {
		t.Errorf("login success row = %+v, want actor %s via password", loginSuccess, aliceID)
	}
	if logout == nil || logout.ActorID == nil || *logout.ActorID != aliceID || logout.Via != "session" || logout.CorrelationID != logoutCorr {
		t.Errorf("logout row = %+v, want actor %s via session correlation %q", logout, aliceID, logoutCorr)
	}

	// ---------------------------------------------------------------------
	// Phase 2 — governance + membership rows, and the org Activity feed.
	// ---------------------------------------------------------------------

	// Alice creates the organization.
	resp = alice.do(t, http.MethodPost, "/api/v1/organizations",
		`{"slug":"audit-labs","name":"Audit Research","description":"lab"}`)
	orgCorr := resp.Header.Get(observability.HeaderCorrelationID)
	mustStatus(t, resp, http.StatusCreated)
	var created orgResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("create org payload: %v", err)
	}
	orgID := created.Organization.ID

	// Rename it (org.updated).
	resp = alice.do(t, http.MethodPatch, "/api/v1/organizations/"+orgID,
		`{"name":"Audit Research Lab"}`)
	mustStatus(t, resp, http.StatusOK)

	// Invite bob (org.member.invited).
	resp = alice.do(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/members",
		`{"handle":"audit-bob","role":"contributor"}`)
	mustStatus(t, resp, http.StatusCreated)

	// Adjust bob's membership (org.member.updated).
	resp = alice.do(t, http.MethodPatch, "/api/v1/organizations/"+orgID+"/members/"+bobID,
		`{"role":"maintainer","verified":true}`)
	mustStatus(t, resp, http.StatusOK)

	// A non-owner governance write is refused — and must leave no row.
	resp = carol.do(t, http.MethodDelete, "/api/v1/organizations/"+orgID, "")
	mustStatus(t, resp, http.StatusForbidden)
	mustEnvelope(t, resp, orgs.CodeOrgForbidden)

	// Bob leaves (org.member.removed).
	resp = bob.do(t, http.MethodDelete, "/api/v1/organizations/"+orgID+"/members/"+bobID, "")
	mustStatus(t, resp, http.StatusNoContent)

	orgRows := sqlAuditRows(t, ctx, pool, "org", orgID)
	// org.created + org.updated + member.invited + member.updated +
	// member.removed — the refused deactivate left no row.
	if len(orgRows) != 5 {
		t.Fatalf("org audit rows = %d, want 5: %+v", len(orgRows), orgRows)
	}
	wantActions := []string{"org.created", "org.updated", "org.member.invited", "org.member.updated", "org.member.removed"}
	// The membership-removed row's actor is bob (he left); every other
	// governance row was alice acting.
	wantActors := []string{aliceID, aliceID, aliceID, aliceID, bobID}
	for i, want := range wantActions {
		if orgRows[i].Action != want {
			t.Errorf("org row %d action = %q, want %q", i, orgRows[i].Action, want)
		}
		if orgRows[i].OrganizationID == nil || *orgRows[i].OrganizationID != orgID {
			t.Errorf("org row %d organization_id = %v, want %s", i, orgRows[i].OrganizationID, orgID)
		}
		if orgRows[i].ActorID == nil || *orgRows[i].ActorID != wantActors[i] {
			t.Errorf("org row %d actor = %v, want %s", i, orgRows[i].ActorID, wantActors[i])
		}
		if orgRows[i].Via != "session" {
			t.Errorf("org row %d via = %q, want session", i, orgRows[i].Via)
		}
		if orgRows[i].CorrelationID == "" {
			t.Errorf("org row %d: empty correlation id", i)
		}
	}
	if orgRows[0].CorrelationID != orgCorr {
		t.Errorf("org.created correlation = %q, want the create request's %q", orgRows[0].CorrelationID, orgCorr)
	}
	if orgRows[0].AfterSummary == nil || !json.Valid([]byte(*orgRows[0].AfterSummary)) ||
		!strings.Contains(*orgRows[0].AfterSummary, "audit-labs") {
		t.Errorf("org.created after_summary = %v, want jsonb with slug", orgRows[0].AfterSummary)
	}
	if orgRows[1].BeforeSummary == nil || orgRows[1].AfterSummary == nil {
		t.Errorf("org.updated must carry before/after summaries: %+v", orgRows[1])
	}

	// The org Activity feed over the wire: alice (member) reads it.
	feed := mustActivity(t, alice, "/api/v1/organizations/"+orgID+"/activity")
	if len(feed.Entries) != 5 {
		t.Fatalf("org activity entries = %d, want 5", len(feed.Entries))
	}
	// Newest first: member.removed on top, created at the bottom.
	if feed.Entries[0].Action != "org.member.removed" || feed.Entries[4].Action != "org.created" {
		t.Errorf("activity order = %v ... %v, want member.removed ... created",
			feed.Entries[0].Action, feed.Entries[4].Action)
	}
	for i, e := range feed.Entries {
		if e.ActorHandle == nil || *e.ActorHandle != "audit-alice" && *e.ActorHandle != "audit-bob" {
			t.Errorf("entry %d actor_handle = %v", i, e.ActorHandle)
		}
		if e.OrganizationID == nil || *e.OrganizationID != orgID {
			t.Errorf("entry %d organization_id = %v, want %s", i, e.OrganizationID, orgID)
		}
		if e.CorrelationID == "" {
			t.Errorf("entry %d: empty correlation id on the wire", i)
		}
	}

	// Visibility: bob (affiliation ended) and carol (never a member) get
	// the owning surface's not-found; anonymous gets 401.
	for _, uc := range []*testUserClient{bob, carol} {
		resp := uc.do(t, http.MethodGet, "/api/v1/organizations/"+orgID+"/activity", "")
		mustStatus(t, resp, http.StatusNotFound)
		mustEnvelope(t, resp, orgs.CodeOrgNotFound)
	}
	anon := newTestUserClient(ts.URL)
	resp = anon.do(t, http.MethodGet, "/api/v1/organizations/"+orgID+"/activity", "")
	mustStatus(t, resp, http.StatusUnauthorized)
	mustEnvelope(t, resp, authn.CodeUnauthenticated)

	// ---------------------------------------------------------------------
	// Phase 3 — keyset pagination: limit 3 over 5 rows, no dupes, no loss.
	// ---------------------------------------------------------------------
	seen := map[string]bool{}
	cursor := ""
	for {
		path := "/api/v1/organizations/" + orgID + "/activity?limit=3"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		page := mustActivity(t, alice, path)
		if len(page.Entries) == 0 || len(page.Entries) > 3 {
			t.Fatalf("page size = %d, want 1..3", len(page.Entries))
		}
		for _, e := range page.Entries {
			if seen[e.ID] {
				t.Fatalf("duplicate entry %s across pages", e.ID)
			}
			seen[e.ID] = true
		}
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	if len(seen) != 5 {
		t.Errorf("paged through %d distinct entries, want 5", len(seen))
	}

	// A malformed cursor and a malformed limit are validation errors, not
	// store errors.
	resp = alice.do(t, http.MethodGet, "/api/v1/organizations/"+orgID+"/activity?cursor=!!!", "")
	mustStatus(t, resp, http.StatusBadRequest)
	mustEnvelope(t, resp, "VALIDATION_FAILED")
	// A parseable timestamp with a non-uuid id — and an id smuggling the
	// separator — must be 400, never a store round-trip (B1/M2 regression:
	// these once reached the persistence layer and panicked the handler).
	for _, forged := range []string{
		auditCursorToken("2026-09-12T10:00:00Z|zzz"),
		auditCursorToken("2026-09-12T10:00:00Z|a|b"),
	} {
		resp = alice.do(t, http.MethodGet, "/api/v1/organizations/"+orgID+"/activity?cursor="+url.QueryEscape(forged), "")
		mustStatus(t, resp, http.StatusBadRequest)
		mustEnvelope(t, resp, "VALIDATION_FAILED")
	}
	resp = alice.do(t, http.MethodGet, "/api/v1/organizations/"+orgID+"/activity?limit=abc", "")
	mustStatus(t, resp, http.StatusBadRequest)
	mustEnvelope(t, resp, "VALIDATION_FAILED")

	// ---------------------------------------------------------------------
	// Phase 4 — project.created with visibility summary + project feed.
	// ---------------------------------------------------------------------
	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"audit-mof","name":"Audit MOF","purpose":"screening","visibility":"private","organization_id":"`+orgID+`"}`)
	projectCorr := resp.Header.Get(observability.HeaderCorrelationID)
	mustStatus(t, resp, http.StatusCreated)
	var projectCreated projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&projectCreated); err != nil {
		t.Fatalf("create project payload: %v", err)
	}
	projectID := projectCreated.Project.ID

	projectRows := sqlAuditRows(t, ctx, pool, "project", projectID)
	if len(projectRows) != 1 {
		t.Fatalf("project audit rows = %d, want 1", len(projectRows))
	}
	pr := projectRows[0]
	if pr.Action != "project.created" || pr.Via != "session" ||
		pr.ActorID == nil || *pr.ActorID != aliceID ||
		pr.ProjectID == nil || *pr.ProjectID != projectID ||
		pr.OrganizationID == nil || *pr.OrganizationID != orgID {
		t.Errorf("project.created row = %+v", pr)
	}
	if pr.CorrelationID != projectCorr {
		t.Errorf("project.created correlation = %q, want %q", pr.CorrelationID, projectCorr)
	}
	if pr.AfterSummary == nil || !strings.Contains(*pr.AfterSummary, "private") {
		t.Errorf("project.created after_summary = %v, want the visibility preset", pr.AfterSummary)
	}

	// The project feed over the wire: the creator reads it; a non-member
	// gets the owning surface's not-found; anonymous gets 401.
	feed = mustActivity(t, alice, "/api/v1/projects/"+projectID+"/activity")
	if len(feed.Entries) != 1 || feed.Entries[0].Action != "project.created" {
		t.Fatalf("project activity = %+v", feed)
	}
	if feed.Entries[0].ProjectID == nil || *feed.Entries[0].ProjectID != projectID ||
		feed.Entries[0].OrganizationID == nil || *feed.Entries[0].OrganizationID != orgID {
		t.Errorf("project entry scope = %+v", feed.Entries[0])
	}
	if !strings.Contains(string(feed.Entries[0].AfterSummary), `"visibility":"private"`) {
		t.Errorf("wire after_summary = %s", feed.Entries[0].AfterSummary)
	}
	resp = carol.do(t, http.MethodGet, "/api/v1/projects/"+projectID+"/activity", "")
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, projects.CodeProjectNotFound)
	resp = anon.do(t, http.MethodGet, "/api/v1/projects/"+projectID+"/activity", "")
	mustStatus(t, resp, http.StatusUnauthorized)

	// ---------------------------------------------------------------------
	// Phase 5 — no update/delete audit endpoint: every state-changing verb
	// on the activity paths answers 405 at routing (session + CSRF valid,
	// so the guard is not what refuses them).
	// ---------------------------------------------------------------------
	for _, path := range []string{
		"/api/v1/projects/" + projectID + "/activity",
		"/api/v1/organizations/" + orgID + "/activity",
	} {
		for _, method := range []string{http.MethodPatch, http.MethodPut, http.MethodDelete} {
			resp := alice.do(t, method, path, "")
			mustStatus(t, resp, http.StatusMethodNotAllowed)
		}
		// HEAD mirrors GET (200, no body): the read-only rule admits the
		// read-only verb.
		resp := alice.do(t, http.MethodHead, path, "")
		mustStatus(t, resp, http.StatusOK)
	}
}
