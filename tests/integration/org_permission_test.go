// Package integration — T0103-TEST-01 "org permission integration"
// (blocking): the organization governance rules exercised end to end
// against REAL PostgreSQL — real migrations, real pgx stores, the real
// auth guard (session + CSRF) and the real orgshttp handlers, exactly as
// cmd/api/main.go composes them. Only the session store is in-memory
// (Redis semantics are orthogonal to org permission and already covered by
// the auth e2e suite).
//
// The acceptance criteria each map to a phase of the test:
//
//   - "Owner 可邀请/调整 role"      → TestOrgPermissionIntegration: owner
//     invites, adjusts role/verified/start; non-owner attempts are refused.
//   - "普通成员不能提升自己"         → TestOrgPermissionIntegration: member
//     PATCHes own role to owner → 403; non-owner governance writes → 403.
//   - "离职不删除历史"              → TestOrgPermissionIntegration: member
//     leaves; the membership row still exists with affiliation_end set
//     (verified with SQL against the same database).
package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/orgshttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

const orgTaskID = "T0103"

// todayUTC is the local calendar date at UTC midnight — the same
// truncation the org service applies to affiliation dates (its dateOnly
// helper is unexported), so assertions are timezone-independent.
func todayUTC() time.Time {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// testUserClient is one browser: session cookies + the CSRF token bound to
// its session.
type testUserClient struct {
	client *http.Client
	server string
	csrf   string
}

func newTestUserClient(server string) *testUserClient {
	jar, _ := cookiejar.New(nil)
	return &testUserClient{
		client: &http.Client{
			Jar:           jar,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		server: server,
	}
}

// do sends a request from this browser. The CSRF header rides along on
// every request — harmless on reads, required on writes. The body is
// closed in test cleanup; helpers that decode drain it first.
func (uc *testUserClient) do(t *testing.T, method, path, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, uc.server+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-CSRF-Token", uc.csrf)
	resp, err := uc.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// signup creates an account through the real signup endpoint (real users
// table, session in the in-memory store) and returns the authenticated
// browser plus the user's ID.
func signup(t *testing.T, server, email, handle string) (*testUserClient, string) {
	t.Helper()
	uc := newTestUserClient(server)
	body := fmt.Sprintf(`{"email":%q,"password":"long-enough-password-1","handle":%q,"display_name":%q}`,
		email, handle, handle)
	req, err := http.NewRequest(http.MethodPost, server+"/api/v1/auth/signup", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := uc.client.Do(req)
	if err != nil {
		t.Fatalf("signup %s: %v", handle, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("signup %s = %d: %s", handle, resp.StatusCode, readAll(t, resp))
	}
	var payload struct {
		CSRFToken string `json:"csrf_token"`
		User      struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("signup %s payload: %v", handle, err)
	}
	uc.csrf = payload.CSRFToken
	return uc, payload.User.ID
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

type errorEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func mustStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("%s %s = %d, want %d: %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, want, readAll(t, resp))
	}
}

func mustEnvelope(t *testing.T, resp *http.Response, wantCode string) {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal([]byte(readAll(t, resp)), &env); err != nil {
		t.Fatalf("envelope decode: %v", err)
	}
	if env.Code != wantCode {
		t.Fatalf("code = %q, want %q", env.Code, wantCode)
	}
}

type orgResponse struct {
	Organization struct {
		ID          string `json:"id"`
		Slug        string `json:"slug"`
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"organization"`
	Membership struct {
		UserID           string `json:"user_id"`
		Role             string `json:"role"`
		AffiliationStart string `json:"affiliation_start"`
		Verified         bool   `json:"verified"`
	} `json:"membership"`
}

type membershipResponse struct {
	OrganizationID   string  `json:"organization_id"`
	UserID           string  `json:"user_id"`
	Role             string  `json:"role"`
	AffiliationStart string  `json:"affiliation_start"`
	AffiliationEnd   *string `json:"affiliation_end"`
	Verified         bool    `json:"verified"`
}

// TestOrgPermissionIntegration is the T0103-TEST-01 "org permission
// integration" gate. It drives the full acceptance journey over the wire
// against real PostgreSQL.
func TestOrgPermissionIntegration(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), orgTaskID)

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
	orgAPI := orgshttp.New(orgshttp.Deps{Store: persistence.NewOrgStore(pool)})
	apiMux := http.NewServeMux()
	apiMux.Handle("/api/v1/auth/", authAPI.Routes())
	// Same dual registration as cmd/api/main.go: the bare path must not
	// be redirected for a trailing slash.
	apiMux.Handle("/api/v1/organizations", orgAPI.Routes())
	apiMux.Handle("/api/v1/organizations/", orgAPI.Routes())
	ts := httptest.NewServer(authAPI.Guard(apiMux))
	defer ts.Close()

	alice, aliceID := signup(t, ts.URL, "org-alice@example.com", "org-alice")
	bob, bobID := signup(t, ts.URL, "org-bob@example.com", "org-bob")
	carol, _ := signup(t, ts.URL, "org-carol@example.com", "org-carol")

	// --- anonymous write is 401 at the guard, before any routing ---
	anon := newTestUserClient(ts.URL)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/organizations", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := anon.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	mustStatus(t, resp, http.StatusUnauthorized)
	_ = resp.Body.Close()

	// --- alice creates the organization; she becomes its verified owner ---
	resp = alice.do(t, http.MethodPost, "/api/v1/organizations",
		`{"slug":"acme-labs","name":"Acme Research","description":"lab"}`)
	mustStatus(t, resp, http.StatusCreated)
	var created orgResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("create payload: %v", err)
	}
	if created.Membership.Role != "owner" || !created.Membership.Verified {
		t.Fatalf("creator membership = %+v, want verified owner", created.Membership)
	}
	if created.Membership.UserID != aliceID || created.Organization.ID == "" {
		t.Fatalf("create response ids = %+v (alice %s)", created, aliceID)
	}
	if created.Organization.Slug != "acme-labs" {
		t.Errorf("slug = %q", created.Organization.Slug)
	}
	orgID := created.Organization.ID

	// --- slug conflict ---
	resp = alice.do(t, http.MethodPost, "/api/v1/organizations",
		`{"slug":"acme-labs","name":"Another"}`)
	mustStatus(t, resp, http.StatusConflict)
	mustEnvelope(t, resp, "ORG_SLUG_TAKEN")

	// --- rework M1: PATCH must apply partial updates, not wipe the
	// untouched field ---
	// (GET/PATCH return the bare org payload; only create wraps it.)
	type orgPayloadResp struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	// Name-only PATCH: the description must survive.
	resp = alice.do(t, http.MethodPatch, "/api/v1/organizations/"+orgID,
		`{"name":"Acme Research Renamed"}`)
	mustStatus(t, resp, http.StatusOK)
	var renamed orgPayloadResp
	if err := json.NewDecoder(resp.Body).Decode(&renamed); err != nil {
		t.Fatalf("rename payload: %v", err)
	}
	if renamed.Name != "Acme Research Renamed" {
		t.Errorf("name after PATCH = %q", renamed.Name)
	}
	if renamed.Description != "lab" {
		t.Errorf("description after name-only PATCH = %q, want %q (untouched field wiped)", renamed.Description, "lab")
	}
	// Description-only PATCH: valid, and the name must survive.
	resp = alice.do(t, http.MethodPatch, "/api/v1/organizations/"+orgID,
		`{"description":"renewed lab"}`)
	mustStatus(t, resp, http.StatusOK)
	var redescribed orgPayloadResp
	if err := json.NewDecoder(resp.Body).Decode(&redescribed); err != nil {
		t.Fatalf("redescribe payload: %v", err)
	}
	if redescribed.Name != "Acme Research Renamed" {
		t.Errorf("name after description-only PATCH = %q, want unchanged", redescribed.Name)
	}
	if redescribed.Description != "renewed lab" {
		t.Errorf("description after PATCH = %q, want %q", redescribed.Description, "renewed lab")
	}

	// --- non-member cannot see the organization (existence hiding) ---
	resp = bob.do(t, http.MethodGet, "/api/v1/organizations/"+orgID, "")
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, "ORG_NOT_FOUND")

	// --- owner invites bob as contributor (affiliation starts today,
	// unverified) ---
	resp = alice.do(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/members",
		`{"handle":"org-bob","role":"contributor"}`)
	mustStatus(t, resp, http.StatusCreated)
	var invited membershipResponse
	if err := json.NewDecoder(resp.Body).Decode(&invited); err != nil {
		t.Fatalf("invite payload: %v", err)
	}
	if invited.Role != "contributor" || invited.Verified {
		t.Fatalf("invite = %+v, want unverified contributor", invited)
	}
	if want := todayUTC().Format("2006-01-02"); invited.AffiliationStart != want {
		t.Errorf("affiliation_start = %q, want today %q", invited.AffiliationStart, want)
	}
	if invited.UserID != bobID {
		t.Errorf("invited user = %q, want bob %q", invited.UserID, bobID)
	}

	// --- bob is now a member: reads work, governance does not ---
	resp = bob.do(t, http.MethodGet, "/api/v1/organizations/"+orgID, "")
	mustStatus(t, resp, http.StatusOK)

	// Acceptance: 普通成员不能提升自己 — bob promotes himself to owner.
	resp = bob.do(t, http.MethodPatch, "/api/v1/organizations/"+orgID+"/members/"+bobID,
		`{"role":"owner"}`)
	mustStatus(t, resp, http.StatusForbidden)
	mustEnvelope(t, resp, "ORG_FORBIDDEN")

	// A non-owner cannot adjust anyone else either (alice's role).
	resp = bob.do(t, http.MethodPatch, "/api/v1/organizations/"+orgID+"/members/"+aliceID,
		`{"role":"viewer"}`)
	mustStatus(t, resp, http.StatusForbidden)
	mustEnvelope(t, resp, "ORG_FORBIDDEN")

	// A non-owner cannot invite.
	resp = bob.do(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/members",
		`{"handle":"org-carol","role":"viewer"}`)
	mustStatus(t, resp, http.StatusForbidden)
	mustEnvelope(t, resp, "ORG_FORBIDDEN")

	// A non-owner cannot update or deactivate the organization.
	resp = bob.do(t, http.MethodPatch, "/api/v1/organizations/"+orgID, `{"name":"Hijacked"}`)
	mustStatus(t, resp, http.StatusForbidden)
	resp = bob.do(t, http.MethodDelete, "/api/v1/organizations/"+orgID, "")
	mustStatus(t, resp, http.StatusForbidden)

	// Acceptance: Owner 可邀请/调整 role — alice adjusts bob's role to
	// maintainer and verifies his affiliation.
	resp = alice.do(t, http.MethodPatch, "/api/v1/organizations/"+orgID+"/members/"+bobID,
		`{"role":"maintainer","verified":true}`)
	mustStatus(t, resp, http.StatusOK)
	var adjusted membershipResponse
	if err := json.NewDecoder(resp.Body).Decode(&adjusted); err != nil {
		t.Fatalf("adjust payload: %v", err)
	}
	if adjusted.Role != "maintainer" || !adjusted.Verified {
		t.Fatalf("adjusted = %+v, want verified maintainer", adjusted)
	}

	// Even a maintainer cannot promote himself (maintainer governs
	// projects, not organizations).
	resp = bob.do(t, http.MethodPatch, "/api/v1/organizations/"+orgID+"/members/"+bobID,
		`{"role":"owner"}`)
	mustStatus(t, resp, http.StatusForbidden)
	mustEnvelope(t, resp, "ORG_FORBIDDEN")

	// The owner may appoint a co-owner.
	resp = alice.do(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/members",
		`{"handle":"org-carol","role":"owner"}`)
	mustStatus(t, resp, http.StatusCreated)
	var carolInvite membershipResponse
	if err := json.NewDecoder(resp.Body).Decode(&carolInvite); err != nil {
		t.Fatalf("carol invite payload: %v", err)
	}
	carolID := carolInvite.UserID

	// Inviting an unknown handle is a clear 404.
	resp = alice.do(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/members",
		`{"handle":"no-such-user","role":"viewer"}`)
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, "USER_NOT_FOUND")

	// Inviting the same user twice is a conflict.
	resp = alice.do(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/members",
		`{"handle":"org-bob","role":"viewer"}`)
	mustStatus(t, resp, http.StatusConflict)
	mustEnvelope(t, resp, "MEMBER_ALREADY_EXISTS")

	// --- acceptance: 离职不删除历史 — bob leaves; the row stays ---
	resp = bob.do(t, http.MethodDelete, "/api/v1/organizations/"+orgID+"/members/"+bobID, "")
	mustStatus(t, resp, http.StatusNoContent)

	// Verified with SQL against the SAME database: the membership row
	// still exists and carries the affiliation_end date.
	var role string
	var end *time.Time
	var verified bool
	err = pool.QueryRow(ctx,
		`SELECT role, affiliation_end, verified
		   FROM organization_memberships
		  WHERE organization_id = $1 AND user_id = $2`, orgID, bobID).
		Scan(&role, &end, &verified)
	if err != nil {
		t.Fatalf("membership row must survive the leave: %v", err)
	}
	if end == nil {
		t.Fatal("affiliation_end must be set after leaving")
	}
	if want := todayUTC(); !end.UTC().Equal(want) {
		t.Errorf("affiliation_end = %v, want today %v", end, want)
	}
	if role != "maintainer" {
		t.Errorf("role after leave = %q (kept for history), want %q", role, "maintainer")
	}

	// The ex-member no longer sees the organization; the owner still sees
	// the historical membership in the member list.
	resp = bob.do(t, http.MethodGet, "/api/v1/organizations/"+orgID, "")
	mustStatus(t, resp, http.StatusNotFound)
	resp = alice.do(t, http.MethodGet, "/api/v1/organizations/"+orgID+"/members", "")
	mustStatus(t, resp, http.StatusOK)
	var memberList struct {
		Members []membershipResponse `json:"members"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&memberList); err != nil {
		t.Fatalf("member list payload: %v", err)
	}
	foundBob := false
	for _, m := range memberList.Members {
		if m.UserID == bobID {
			foundBob = true
			if m.AffiliationEnd == nil {
				t.Error("bob's historical membership must carry affiliation_end")
			}
		}
	}
	if !foundBob {
		t.Error("bob's membership must remain listed (history is not deleted)")
	}

	// --- rework B1: adjusting an ended membership must never clear the
	// departure date (affiliation_end is EndAffiliation's exclusive job) ---
	firstEnd := *end
	resp = alice.do(t, http.MethodPatch, "/api/v1/organizations/"+orgID+"/members/"+bobID,
		`{"verified":true}`)
	mustStatus(t, resp, http.StatusOK)
	var afterLeaveAdjust membershipResponse
	if err := json.NewDecoder(resp.Body).Decode(&afterLeaveAdjust); err != nil {
		t.Fatalf("post-leave adjust payload: %v", err)
	}
	if afterLeaveAdjust.AffiliationEnd == nil {
		t.Fatal("adjusting an ended membership cleared affiliation_end — the departure date must survive")
	}
	if *afterLeaveAdjust.AffiliationEnd != firstEnd.Format("2006-01-02") {
		t.Errorf("affiliation_end after adjustment = %q, want the original %q",
			*afterLeaveAdjust.AffiliationEnd, firstEnd.Format("2006-01-02"))
	}

	// --- minor fix: a repeated remove is a no-op — the historical end
	// date is not re-stamped ---
	resp = alice.do(t, http.MethodDelete, "/api/v1/organizations/"+orgID+"/members/"+bobID, "")
	mustStatus(t, resp, http.StatusNoContent)
	var endAfterRepeat *time.Time
	err = pool.QueryRow(ctx,
		`SELECT affiliation_end FROM organization_memberships
		  WHERE organization_id = $1 AND user_id = $2`, orgID, bobID).
		Scan(&endAfterRepeat)
	if err != nil {
		t.Fatalf("membership row must survive the repeated remove: %v", err)
	}
	if endAfterRepeat == nil || !endAfterRepeat.UTC().Equal(firstEnd.UTC()) {
		t.Errorf("affiliation_end after repeated remove = %v, want the original %v", endAfterRepeat, firstEnd)
	}

	// --- last-owner protection ---
	// Carol (co-owner) may leave — alice remains.
	resp = carol.do(t, http.MethodDelete, "/api/v1/organizations/"+orgID+"/members/"+carolID, "")
	mustStatus(t, resp, http.StatusNoContent)

	// Alice is now the last owner: she can neither leave nor demote
	// herself.
	resp = alice.do(t, http.MethodDelete, "/api/v1/organizations/"+orgID+"/members/"+aliceID, "")
	mustStatus(t, resp, http.StatusConflict)
	mustEnvelope(t, resp, "LAST_OWNER")
	resp = alice.do(t, http.MethodPatch, "/api/v1/organizations/"+orgID+"/members/"+aliceID,
		`{"role":"viewer"}`)
	mustStatus(t, resp, http.StatusConflict)
	mustEnvelope(t, resp, "LAST_OWNER")

	// --- deactivation freezes governance but keeps history readable ---
	resp = alice.do(t, http.MethodDelete, "/api/v1/organizations/"+orgID, "")
	mustStatus(t, resp, http.StatusNoContent)

	resp = alice.do(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/members",
		`{"handle":"org-carol","role":"viewer"}`)
	mustStatus(t, resp, http.StatusConflict)
	mustEnvelope(t, resp, "ORG_DEACTIVATED")

	// Members can still read their deactivated organization.
	resp = alice.do(t, http.MethodGet, "/api/v1/organizations/"+orgID, "")
	mustStatus(t, resp, http.StatusOK)

	// The organization list reflects alice's membership.
	resp = alice.do(t, http.MethodGet, "/api/v1/organizations", "")
	mustStatus(t, resp, http.StatusOK)
}
