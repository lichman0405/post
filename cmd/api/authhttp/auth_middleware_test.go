package authhttp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/authn/oidctest"
	"github.com/lichman0405/post/internal/persistence/memstore"
)

// The transport tests drive the REAL guard + handlers (newAuthAPI) over
// memory adapters. Storage is fake; every policy line in the middleware is
// production code.

// newTestAPI builds the production auth API over memory adapters.
func newTestAPI(t *testing.T, cfg authn.Config, oidc authn.OIDCProvider) (*API, *memstore.Users) {
	t.Helper()
	users := memstore.NewUsers()
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = time.Hour
	}
	if cfg.WebOrigin == "" {
		cfg.WebOrigin = "http://web.test"
	}
	if cfg.LoginLimitPerEmail == 0 {
		cfg.LoginLimitPerEmail = 5
	}
	if cfg.LoginLimitPerIP == 0 {
		cfg.LoginLimitPerIP = 1000
	}
	if cfg.SignupLimitPerIP == 0 {
		cfg.SignupLimitPerIP = 1000
	}
	if cfg.LoginWindow == 0 {
		cfg.LoginWindow = time.Minute
	}
	return New(Deps{
		Users:      users,
		Sessions:   memstore.NewSessions(),
		Limiter:    memstore.NewLimiter(),
		OIDCClient: oidc,
		Cfg:        cfg,
		Secure:     false,
	}), users
}

// doAuth issues a request against the auth API subtree. A body implies
// JSON content type (like a real fetch call with a JSON string).
func doAuth(t *testing.T, h http.Handler, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	req.RemoteAddr = "192.0.2.10:34567"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func cookieValue(rec *httptest.ResponseRecorder, name string) string {
	for _, c := range (&http.Response{Header: rec.Header()}).Cookies() {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

func seedUserHTTP(t *testing.T, users *memstore.Users, email, password string) {
	t.Helper()
	hash, err := authn.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if _, err := users.CreateWithPassword(context.Background(), email, hash, email, email); err != nil {
		t.Fatalf("seed user: %v", err)
	}
}

const (
	apiTarget  = "http://api.test"
	signupJSON = `{"email":"alice@example.com","password":"long-enough-password-1"}`
)

// TestUnauthenticatedWriteReturns401 is the acceptance criterion
// "未认证写 API → 401": ANY state-changing request under /api/v1 without a
// session answers 401 — structurally, before routing, so even an
// unimplemented product endpoint (POST /api/v1/projects) is 401, not 404.
func TestUnauthenticatedWriteReturns401(t *testing.T) {
	api, _ := newTestAPI(t, authn.Config{}, nil)
	h := api.Routes()

	cases := []struct{ method, path string }{
		{http.MethodPost, "/api/v1/projects"},
		{http.MethodPut, "/api/v1/projects/123"},
		{http.MethodPatch, "/api/v1/projects/123"},
		{http.MethodDelete, "/api/v1/projects/123"},
		{http.MethodPost, "/api/v1/auth/logout"},
	}
	for _, tc := range cases {
		rec := doAuth(t, h, tc.method, apiTarget+tc.path, "{}", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without session = %d, want 401", tc.method, tc.path, rec.Code)
			continue
		}
		var env errorEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Code != authn.CodeUnauthenticated {
			t.Errorf("%s %s envelope = %s (%v), want code %s", tc.method, tc.path, rec.Body, err, authn.CodeUnauthenticated)
		}
	}
}

// TestUnauthenticatedReadPassesToRouting: reads are not default-deny — the
// guard lets them through to the handler (which decides; whoami answers
// 401, an unknown route answers the mux 404).
func TestUnauthenticatedReadPassesToRouting(t *testing.T) {
	api, _ := newTestAPI(t, authn.Config{}, nil)
	h := api.Routes()

	rec := doAuth(t, h, http.MethodGet, apiTarget+"/api/v1/auth/session", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET session without session = %d, want 401 (handler's own answer)", rec.Code)
	}
	rec = doAuth(t, h, http.MethodGet, apiTarget+"/api/v1/projects", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET unknown read = %d, want 404 (guard must not block reads)", rec.Code)
	}
}

// TestSignupLoginLogoutFlowHTTP is the login/logout E2E acceptance
// criterion at the transport layer: cookie issued on signup, whoami
// resolves it, CSRF-bound logout revokes it, whoami then answers 401.
func TestSignupLoginLogoutFlowHTTP(t *testing.T) {
	api, _ := newTestAPI(t, authn.Config{}, nil)
	h := api.Routes()

	rec := doAuth(t, h, http.MethodPost, apiTarget+"/api/v1/auth/signup", signupJSON, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("signup = %d: %s", rec.Code, rec.Body)
	}
	sessionToken := cookieValue(rec, "post_session")
	if sessionToken == "" {
		t.Fatal("signup did not set the post_session cookie")
	}
	var payload sessionPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("signup body: %v", err)
	}
	if payload.CSRFToken == "" || payload.User.Email != "alice@example.com" {
		t.Fatalf("signup payload = %+v", payload)
	}

	// Whoami with the cookie.
	rec = doAuth(t, h, http.MethodGet, apiTarget+"/api/v1/auth/session", "",
		map[string]string{"Cookie": "post_session=" + sessionToken})
	if rec.Code != http.StatusOK {
		t.Fatalf("session = %d: %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || payload.User.Email != "alice@example.com" {
		t.Fatalf("session body = %s (%v)", rec.Body, err)
	}

	// Logout requires the CSRF token from the login response.
	rec = doAuth(t, h, http.MethodPost, apiTarget+"/api/v1/auth/logout", "",
		map[string]string{
			"Cookie":   "post_session=" + sessionToken,
			headerCSRF: payload.CSRFToken,
		})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout = %d: %s", rec.Code, rec.Body)
	}

	// The revoked session no longer authenticates.
	rec = doAuth(t, h, http.MethodGet, apiTarget+"/api/v1/auth/session", "",
		map[string]string{"Cookie": "post_session=" + sessionToken})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("session after logout = %d, want 401", rec.Code)
	}

	// Login mints a fresh session for the same account.
	loginJSON := `{"email":"alice@example.com","password":"long-enough-password-1"}`
	rec = doAuth(t, h, http.MethodPost, apiTarget+"/api/v1/auth/login", loginJSON, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d: %s", rec.Code, rec.Body)
	}
	if token := cookieValue(rec, "post_session"); token == "" || token == sessionToken {
		t.Error("login must issue a fresh session cookie")
	}
}

// TestCSRFEnforcedOnAllWrites: a valid session without (or with a wrong)
// X-CSRF-Token is 403 on every state change; the right token passes the
// guard (an unimplemented route then answers the mux 404, proving the guard
// let it through).
func TestCSRFEnforcedOnAllWrites(t *testing.T) {
	api, _ := newTestAPI(t, authn.Config{}, nil)
	h := api.Routes()

	rec := doAuth(t, h, http.MethodPost, apiTarget+"/api/v1/auth/signup", signupJSON, nil)
	token := cookieValue(rec, "post_session")
	var payload sessionPayload
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)

	// Missing CSRF header.
	rec = doAuth(t, h, http.MethodPost, apiTarget+"/api/v1/projects", "{}",
		map[string]string{"Cookie": "post_session=" + token})
	if rec.Code != http.StatusForbidden {
		t.Errorf("write without CSRF header = %d, want 403", rec.Code)
	}
	// Wrong CSRF token: same 403 (no oracle).
	rec = doAuth(t, h, http.MethodPost, apiTarget+"/api/v1/projects", "{}",
		map[string]string{"Cookie": "post_session=" + token, headerCSRF: "wrong-token"})
	if rec.Code != http.StatusForbidden {
		t.Errorf("write with wrong CSRF = %d, want 403", rec.Code)
	}
	// Correct CSRF: the guard passes it to routing (route unimplemented -> 404).
	rec = doAuth(t, h, http.MethodPost, apiTarget+"/api/v1/projects", "{}",
		map[string]string{"Cookie": "post_session=" + token, headerCSRF: payload.CSRFToken})
	if rec.Code != http.StatusNotFound {
		t.Errorf("write with valid session+CSRF = %d, want 404 (guard passed, route unimplemented)", rec.Code)
	}
}

// TestLoginCrossSiteAndContentTypeDefenses: the pre-auth login/signup
// writes get Origin + JSON checks — cross-site browser POSTs and HTML-form
// content types are refused before any credential is touched.
func TestLoginCrossSiteAndContentTypeDefenses(t *testing.T) {
	api, users := newTestAPI(t, authn.Config{}, nil)
	seedUserHTTP(t, users, "known@example.com", "right-password-123")
	h := api.Routes()
	loginJSON := `{"email":"known@example.com","password":"right-password-123"}`

	// Cross-site origin.
	rec := doAuth(t, h, http.MethodPost, apiTarget+"/api/v1/auth/login", loginJSON,
		map[string]string{headerOrigin: "https://evil.example"})
	if rec.Code != http.StatusForbidden {
		t.Errorf("login with cross-site Origin = %d, want 403", rec.Code)
	}
	// Sandbox "null" origin.
	rec = doAuth(t, h, http.MethodPost, apiTarget+"/api/v1/auth/login", loginJSON,
		map[string]string{headerOrigin: "null"})
	if rec.Code != http.StatusForbidden {
		t.Errorf("login with null Origin = %d, want 403", rec.Code)
	}
	// Same-site origin passes (no session cookie was set).
	if got := cookieValue(rec, "post_session"); got != "" {
		t.Error("blocked login must not set a session cookie")
	}
	rec = doAuth(t, h, http.MethodPost, apiTarget+"/api/v1/auth/login", loginJSON,
		map[string]string{headerOrigin: apiTarget})
	if rec.Code != http.StatusOK {
		t.Errorf("login with same-site Origin = %d, want 200", rec.Code)
	}
	// Form content type (what an HTML <form> posts) is refused.
	rec = doAuth(t, h, http.MethodPost, apiTarget+"/api/v1/auth/login", loginJSON, nil)
	_ = rec // (JSON content type set by doAuth is fine; the blocked case follows)
	req := httptest.NewRequest(http.MethodPost, apiTarget+"/api/v1/auth/login",
		strings.NewReader("email=x&password=yyyyyyyyyy"))
	req.Header.Set(headerCT, "application/x-www-form-urlencoded")
	req.RemoteAddr = "192.0.2.10:34567"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("login with form content type = %d, want 415", rec.Code)
	}
}

// TestLoginWebOriginAllowedCrossHost: the shipped web app calls the API
// cross-origin (web on one origin, API on another port — the default
// topology), so the pre-auth Origin check must accept exactly the
// configured WebOrigin in addition to the request's own host. Explicit
// allowlist only: never an echo of the request Origin, never "*". This is
// the B1 regression test — before the fix the delivered login page was
// refused with 403 by the guard it shipped with.
func TestLoginWebOriginAllowedCrossHost(t *testing.T) {
	api, users := newTestAPI(t, authn.Config{WebOrigin: "http://web.test"}, nil)
	seedUserHTTP(t, users, "known@example.com", "right-password-123")
	h := api.Routes()
	loginJSON := `{"email":"known@example.com","password":"right-password-123"}`

	// The configured web origin from the API's own host (cross-origin
	// topology): accepted on both pre-auth endpoints.
	rec := doAuth(t, h, http.MethodPost, apiTarget+"/api/v1/auth/login", loginJSON,
		map[string]string{headerOrigin: "http://web.test"})
	if rec.Code != http.StatusOK {
		t.Errorf("login with the configured web Origin from another host = %d: %s, want 200", rec.Code, rec.Body)
	}
	rec = doAuth(t, h, http.MethodPost, apiTarget+"/api/v1/auth/signup", signupJSON,
		map[string]string{headerOrigin: "http://web.test"})
	if rec.Code != http.StatusCreated {
		t.Errorf("signup with the configured web Origin from another host = %d: %s, want 201", rec.Code, rec.Body)
	}

	// A foreign origin is still refused (an allowlist, not a pattern).
	rec = doAuth(t, h, http.MethodPost, apiTarget+"/api/v1/auth/login", loginJSON,
		map[string]string{headerOrigin: "http://evil.test"})
	if rec.Code != http.StatusForbidden {
		t.Errorf("login with foreign Origin = %d, want 403", rec.Code)
	}
	// A look-alike origin sharing a suffix is not the configured origin.
	rec = doAuth(t, h, http.MethodPost, apiTarget+"/api/v1/auth/login", loginJSON,
		map[string]string{headerOrigin: "http://web.test.evil.test"})
	if rec.Code != http.StatusForbidden {
		t.Errorf("login with look-alike Origin = %d, want 403", rec.Code)
	}
}

// TestLoginOriginStrictWithoutWebOrigin: when no WebOrigin is configured
// the pre-auth Origin check keeps its strict same-host behavior (the
// relaxation exists only for the explicitly configured origin).
func TestLoginOriginStrictWithoutWebOrigin(t *testing.T) {
	users := memstore.NewUsers()
	seedUserHTTP(t, users, "known@example.com", "right-password-123")
	api := New(Deps{
		Users:      users,
		Sessions:   memstore.NewSessions(),
		Limiter:    memstore.NewLimiter(),
		OIDCClient: nil,
		Cfg: authn.Config{ // WebOrigin deliberately empty
			SessionTTL:         time.Hour,
			LoginLimitPerEmail: 5,
			LoginLimitPerIP:    1000,
			LoginWindow:        time.Minute,
		},
		Secure: false,
	})
	h := api.Routes()
	rec := doAuth(t, h, http.MethodPost, apiTarget+"/api/v1/auth/login",
		`{"email":"known@example.com","password":"right-password-123"}`,
		map[string]string{headerOrigin: "http://web.test"})
	if rec.Code != http.StatusForbidden {
		t.Errorf("login with cross-host Origin and no WebOrigin configured = %d, want 403 (strict)", rec.Code)
	}
}

// TestLoginEnumerationIdenticalEnvelopes is the enumeration acceptance
// criterion on the wire: wrong password and unknown email answer the same
// status, code, message and retryability (the request_id is a trace id and
// legitimately differs).
func TestLoginEnumerationIdenticalEnvelopes(t *testing.T) {
	api, users := newTestAPI(t, authn.Config{}, nil)
	seedUserHTTP(t, users, "known@example.com", "right-password-123")
	h := api.Routes()

	wrongPass := doAuth(t, h, http.MethodPost, apiTarget+"/api/v1/auth/login",
		`{"email":"known@example.com","password":"wrong-password-456"}`, nil)
	unknownEmail := doAuth(t, h, http.MethodPost, apiTarget+"/api/v1/auth/login",
		`{"email":"nobody@example.com","password":"wrong-password-456"}`, nil)

	if wrongPass.Code != http.StatusUnauthorized || unknownEmail.Code != http.StatusUnauthorized {
		t.Fatalf("statuses = %d / %d, want 401 / 401", wrongPass.Code, unknownEmail.Code)
	}
	var a, b errorEnvelope
	if err := json.Unmarshal(wrongPass.Body.Bytes(), &a); err != nil {
		t.Fatalf("wrong-pass body: %v", err)
	}
	if err := json.Unmarshal(unknownEmail.Body.Bytes(), &b); err != nil {
		t.Fatalf("unknown-email body: %v", err)
	}
	if a.Code != b.Code || a.Message != b.Message || a.Retryable != b.Retryable {
		t.Errorf("envelopes differ: %+v vs %+v — enumeration leak", a, b)
	}
	if a.Code != authn.CodeInvalidCredentials {
		t.Errorf("code = %q, want %q", a.Code, authn.CodeInvalidCredentials)
	}
	if cookieValue(wrongPass, "post_session") != "" || cookieValue(unknownEmail, "post_session") != "" {
		t.Error("failed logins must not set a session cookie")
	}
}

// TestLoginRateLimit429: after the per-email window is exhausted, every
// attempt — even with the correct password — answers 429 + Retry-After.
func TestLoginRateLimit429(t *testing.T) {
	api, users := newTestAPI(t, authn.Config{LoginLimitPerEmail: 3}, nil)
	seedUserHTTP(t, users, "limited@example.com", "right-password-123")
	h := api.Routes()

	attempt := `{"email":"limited@example.com","password":"wrong-password-%d"}`
	for i := 0; i < 3; i++ {
		rec := doAuth(t, h, http.MethodPost, apiTarget+"/api/v1/auth/login",
			strings.Replace(attempt, "%d", "x", 1), nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i+1, rec.Code)
		}
	}
	rec := doAuth(t, h, http.MethodPost, apiTarget+"/api/v1/auth/login",
		`{"email":"limited@example.com","password":"right-password-123"}`, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("limited attempt = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 must carry Retry-After")
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Code != authn.CodeRateLimited {
		t.Errorf("envelope = %s (%v), want code %s", rec.Body, err, authn.CodeRateLimited)
	}
}

// TestCORSPreflight: the configured web origin gets a credentialed echo; a
// foreign origin gets none; every preflight is answered without touching
// routing.
func TestCORSPreflight(t *testing.T) {
	api, _ := newTestAPI(t, authn.Config{WebOrigin: "http://web.test"}, nil)
	h := api.Routes()

	rec := doAuth(t, h, http.MethodOptions, apiTarget+"/api/v1/auth/login", "",
		map[string]string{headerOrigin: "http://web.test"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight = %d, want 204", rec.Code)
	}
	if rec.Header().Get(headerACOrigin) != "http://web.test" || rec.Header().Get(headerACCred) != "true" {
		t.Errorf("preflight CORS headers = %q / %q", rec.Header().Get(headerACOrigin), rec.Header().Get(headerACCred))
	}

	rec = doAuth(t, h, http.MethodOptions, apiTarget+"/api/v1/auth/login", "",
		map[string]string{headerOrigin: "https://evil.example"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("foreign preflight = %d, want 204 (answered, not allowed)", rec.Code)
	}
	if rec.Header().Get(headerACOrigin) != "" {
		t.Errorf("foreign origin got ACAO %q — CORS allow-list broken", rec.Header().Get(headerACOrigin))
	}
}

// TestSessionCookieSecurityShape pins the cookie flags (HttpOnly,
// SameSite=Lax, Secure off in dev) so a regression cannot leak the token.
func TestSessionCookieSecurityShape(t *testing.T) {
	api, _ := newTestAPI(t, authn.Config{}, nil)
	rec := doAuth(t, api.Routes(), http.MethodPost, apiTarget+"/api/v1/auth/signup", signupJSON, nil)
	cookies := (&http.Response{Header: rec.Header()}).Cookies()
	var session *http.Cookie
	for _, c := range cookies {
		if c.Name == "post_session" {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no post_session cookie")
	}
	if !session.HttpOnly || session.SameSite != http.SameSiteLaxMode || session.Secure {
		t.Errorf("cookie flags = HttpOnly:%v SameSite:%v Secure:%v — want HttpOnly, Lax, no Secure (dev)",
			session.HttpOnly, session.SameSite, session.Secure)
	}
	if session.Path != "/" || session.MaxAge <= 0 {
		t.Errorf("cookie path/maxage = %q/%d", session.Path, session.MaxAge)
	}
}

// TestOIDCTransportFlow: the full browser journey through the HTTP layer —
// authorize-url (state cookie) → callback with the provider's code (state
// match, session issued) → whoami shows the OIDC identity.
func TestOIDCTransportFlow(t *testing.T) {
	provider := oidctest.NewProvider()
	provider.Serve()
	defer provider.Close()

	cfg := authn.Config{
		WebOrigin: "http://web.test",
		OIDC: authn.OIDCConfig{
			Enabled:      true,
			Issuer:       provider.Issuer(),
			ClientID:     oidctest.ClientID,
			RedirectPath: "/api/v1/auth/oidc/callback",
		},
	}
	api, _ := newTestAPI(t, cfg, provider.NewClient(""))
	h := api.Routes()

	rec := doAuth(t, h, http.MethodGet, apiTarget+"/api/v1/auth/oidc/authorize-url", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("authorize-url = %d: %s", rec.Code, rec.Body)
	}
	state := cookieValue(rec, "post_oidc_state")
	if state == "" {
		t.Fatal("authorize-url did not set the state cookie")
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || !strings.HasPrefix(body["authorize_url"], provider.Issuer()) {
		t.Fatalf("authorize-url body = %s (%v)", rec.Body, err)
	}

	code := provider.IssueCode(oidctest.Person{
		Subject: "sub-oidc-alice", Email: "oidc.alice@example.com", EmailVerified: true,
		PreferredUsername: "oidc-alice", Name: "Alice O",
	})
	rec = doAuth(t, h, http.MethodGet,
		apiTarget+"/api/v1/auth/oidc/callback?code="+code+"&state="+state, "",
		map[string]string{"Cookie": "post_oidc_state=" + state})
	if rec.Code != http.StatusFound {
		t.Fatalf("callback = %d: %s", rec.Code, rec.Body)
	}
	if loc := rec.Header().Get("Location"); loc != "http://web.test/" {
		t.Errorf("callback Location = %q, want http://web.test/", loc)
	}
	sessionToken := cookieValue(rec, "post_session")
	if sessionToken == "" {
		t.Fatal("callback did not issue a session cookie")
	}

	rec = doAuth(t, h, http.MethodGet, apiTarget+"/api/v1/auth/session", "",
		map[string]string{"Cookie": "post_session=" + sessionToken})
	if rec.Code != http.StatusOK {
		t.Fatalf("OIDC session = %d: %s", rec.Code, rec.Body)
	}
	var payload sessionPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || payload.User.Email != "oidc.alice@example.com" {
		t.Fatalf("OIDC whoami = %s (%v)", rec.Body, err)
	}
}

// TestOIDCCallbackStateBinding: a mismatched or missing state aborts the
// flow without a session; replaying a completed callback cannot mint a
// second session (the code is one-time).
func TestOIDCCallbackStateBinding(t *testing.T) {
	provider := oidctest.NewProvider()
	provider.Serve()
	defer provider.Close()

	cfg := authn.Config{
		WebOrigin: "http://web.test",
		OIDC: authn.OIDCConfig{
			Enabled: true, Issuer: provider.Issuer(),
			ClientID: oidctest.ClientID, RedirectPath: "/api/v1/auth/oidc/callback",
		},
	}
	api, _ := newTestAPI(t, cfg, provider.NewClient(""))
	h := api.Routes()

	// No state cookie at all.
	rec := doAuth(t, h, http.MethodGet, apiTarget+"/api/v1/auth/oidc/callback?code=x&state=y", "", nil)
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=state_mismatch") {
		t.Errorf("no-state callback Location = %q, want state_mismatch", loc)
	}
	if cookieValue(rec, "post_session") != "" {
		t.Error("no-state callback issued a session")
	}

	// Mismatched state.
	rec = doAuth(t, h, http.MethodGet, apiTarget+"/api/v1/auth/oidc/authorize-url", "", nil)
	state := cookieValue(rec, "post_oidc_state")
	rec = doAuth(t, h, http.MethodGet, apiTarget+"/api/v1/auth/oidc/callback?code=x&state=forged", "",
		map[string]string{"Cookie": "post_oidc_state=" + state})
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=state_mismatch") {
		t.Errorf("mismatched-state callback Location = %q, want state_mismatch", loc)
	}
	if cookieValue(rec, "post_session") != "" {
		t.Error("mismatched-state callback issued a session")
	}

	// Valid flow, then replay the SAME cookie+state+code: the code is
	// single-use at the provider, so the replay redirects to an error and
	// never issues a second session.
	code := provider.IssueCode(oidctest.Person{
		Subject: "sub-replay", Email: "replay@example.com", EmailVerified: true,
		PreferredUsername: "replay", Name: "Replay",
	})
	rec = doAuth(t, h, http.MethodGet, apiTarget+"/api/v1/auth/oidc/callback?code="+code+"&state="+state, "",
		map[string]string{"Cookie": "post_oidc_state=" + state})
	if loc := rec.Header().Get("Location"); loc != "http://web.test/" {
		t.Fatalf("valid callback Location = %q, want http://web.test/", loc)
	}
	replayed := doAuth(t, h, http.MethodGet, apiTarget+"/api/v1/auth/oidc/callback?code="+code+"&state="+state, "",
		map[string]string{"Cookie": "post_oidc_state=" + state})
	if loc := replayed.Header().Get("Location"); !strings.Contains(loc, "error=") || loc == "http://web.test/" {
		t.Errorf("replayed callback Location = %q, want an error redirect", loc)
	}
	if cookieValue(replayed, "post_session") != "" {
		t.Error("replayed callback issued a session")
	}
}

// TestOIDCCallbackFailureKeepsExistingSession: a crafted link to the
// callback (every error path) must NOT clear an existing session cookie —
// otherwise any site can log a signed-in user out by pointing them at the
// callback (logout-CSRF). The failure really deletes the state cookie
// (Max-Age<0), and the session survives on the server.
func TestOIDCCallbackFailureKeepsExistingSession(t *testing.T) {
	provider := oidctest.NewProvider()
	provider.Serve()
	defer provider.Close()

	cfg := authn.Config{
		WebOrigin: "http://web.test",
		OIDC: authn.OIDCConfig{
			Enabled: true, Issuer: provider.Issuer(),
			ClientID: oidctest.ClientID, RedirectPath: "/api/v1/auth/oidc/callback",
		},
	}
	api, _ := newTestAPI(t, cfg, provider.NewClient(""))
	h := api.Routes()

	// A signed-in user (session cookie from a normal signup).
	rec := doAuth(t, h, http.MethodPost, apiTarget+"/api/v1/auth/signup", signupJSON, nil)
	sessionToken := cookieValue(rec, "post_session")
	if sessionToken == "" {
		t.Fatal("signup did not set post_session")
	}

	// A crafted callback link (no state cookie) fails the flow...
	rec = doAuth(t, h, http.MethodGet, apiTarget+"/api/v1/auth/oidc/callback?code=x&state=y", "", nil)
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=state_mismatch") {
		t.Fatalf("callback Location = %q, want state_mismatch", loc)
	}
	// ...and must not send any post_session Set-Cookie (a clearing cookie
	// here logs the user out in their browser — the logout-CSRF vector).
	cookies := (&http.Response{Header: rec.Header()}).Cookies()
	for _, c := range cookies {
		if c.Name == "post_session" {
			t.Error("failed callback cleared the session cookie — logout-CSRF vector")
		}
	}
	// The state cookie is really deleted (Max-Age<0), not just emptied.
	var stateDeleted bool
	for _, c := range cookies {
		if c.Name == "post_oidc_state" && c.MaxAge < 0 {
			stateDeleted = true
		}
	}
	if !stateDeleted {
		t.Error("failed callback did not delete the state cookie (Max-Age<0)")
	}

	// The session survives on the server: the browser keeps its cookie and
	// the user stays signed in.
	rec = doAuth(t, h, http.MethodGet, apiTarget+"/api/v1/auth/session", "",
		map[string]string{"Cookie": "post_session=" + sessionToken})
	if rec.Code != http.StatusOK {
		t.Errorf("whoami after failed callback = %d, want 200 (session must survive)", rec.Code)
	}
}

// TestOIDCUnverifiedEmailRefused: the provider verified nothing → the flow
// redirects to email_not_verified and creates no session.
func TestOIDCUnverifiedEmailRefused(t *testing.T) {
	provider := oidctest.NewProvider()
	provider.Serve()
	defer provider.Close()

	cfg := authn.Config{
		WebOrigin: "http://web.test",
		OIDC: authn.OIDCConfig{
			Enabled: true, Issuer: provider.Issuer(),
			ClientID: oidctest.ClientID, RedirectPath: "/api/v1/auth/oidc/callback",
		},
	}
	api, users := newTestAPI(t, cfg, provider.NewClient(""))
	h := api.Routes()

	rec := doAuth(t, h, http.MethodGet, apiTarget+"/api/v1/auth/oidc/authorize-url", "", nil)
	state := cookieValue(rec, "post_oidc_state")
	code := provider.IssueCode(oidctest.Person{Subject: "sub-unverified", Email: "unverified@example.com", EmailVerified: false})
	rec = doAuth(t, h, http.MethodGet, apiTarget+"/api/v1/auth/oidc/callback?code="+code+"&state="+state, "",
		map[string]string{"Cookie": "post_oidc_state=" + state})
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=email_not_verified") {
		t.Errorf("unverified callback Location = %q, want email_not_verified", loc)
	}
	if cookieValue(rec, "post_session") != "" {
		t.Error("unverified OIDC callback issued a session")
	}
	if _, err := users.FindByEmail(context.Background(), "unverified@example.com"); err == nil {
		t.Error("unverified OIDC login created an account")
	}
}

// TestOIDCNotConfigured: with no provider wired, the endpoints answer
// OIDC_NOT_CONFIGURED, never a panic or a half-built URL.
func TestOIDCNotConfigured(t *testing.T) {
	api, _ := newTestAPI(t, authn.Config{}, nil)
	rec := doAuth(t, api.Routes(), http.MethodGet, apiTarget+"/api/v1/auth/oidc/authorize-url", "", nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("authorize-url without OIDC = %d, want 501", rec.Code)
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Code != authn.CodeOIDCNotConfigured {
		t.Errorf("envelope = %s (%v), want code %s", rec.Body, err, authn.CodeOIDCNotConfigured)
	}
}
