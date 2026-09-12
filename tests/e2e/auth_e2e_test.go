// Package e2e runs the auth surface the way a browser/API client does:
// real HTTP servers and clients, the production middleware + handlers
// (cmd/api/authhttp), the production Redis adapters (miniredis speaking
// real Redis protocol) and a real OIDC IdP on the wire. Nothing here is
// mocked at the handler boundary — only the infra (Redis) is in-process.
//
// T0101-TEST-02 "auth e2e" (blocking): `go test ./tests/e2e -run TestE2E`
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/authn/oidctest"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
)

// e2eEnv is one composed deployment: API server + Redis + (optional) IdP.
type e2eEnv struct {
	api   *httptest.Server
	redis *miniredis.Miniredis
	users *memstore.Users
	// client follows no redirects (the OIDC callback 302s are assertions).
	client *http.Client
}

// newE2EEnv mirrors cmd/api main.go's wiring: same Deps construction, same
// guard + subtree composition — only the storage is in-process. The org
// subtree is absent here (it needs PostgreSQL, the org store has no
// in-memory adapter); org coverage lives in tests/integration against the
// real database.
func newE2EEnv(t *testing.T, cfg authn.Config, oidc authn.OIDCProvider) *e2eEnv {
	t.Helper()
	users := memstore.NewUsers()
	mr := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	api := authhttp.New(authhttp.Deps{
		Users:      users,
		Sessions:   persistence.NewRedisSessionStore(redisClient),
		Limiter:    persistence.NewRedisRateLimiter(redisClient),
		OIDCClient: oidc,
		Cfg:        cfg,
		Secure:     false,
	})
	apiMux := http.NewServeMux()
	apiMux.Handle("/api/v1/auth/", api.Routes())
	ts := httptest.NewServer(api.Guard(apiMux))
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = redisClient.Close() })
	jar, _ := cookiejar.New(nil)
	return &e2eEnv{
		api:   ts,
		redis: mr,
		users: users,
		client: &http.Client{
			Jar:           jar,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// defaultCfg is the dev-shaped config with fast limits.
func defaultCfg() authn.Config {
	return authn.Config{
		WebOrigin:          "http://web.test",
		SessionTTL:         time.Hour,
		LoginLimitPerEmail: 5,
		LoginLimitPerIP:    1000,
		LoginWindow:        time.Minute,
		SignupLimitPerIP:   1000,
	}
}

// do issues one request against the env's API. A non-empty body implies
// JSON content type.
func (e *e2eEnv) do(t *testing.T, method, path, body string, headers map[string]string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.api.URL+path, rdr)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func bodyBytes(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b
}

// envelope is the wire error shape (matches authhttp.errorEnvelope).
type envelope struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Retryable bool   `json:"retryable"`
}

func decodeEnvelope(t *testing.T, resp *http.Response) envelope {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(bodyBytes(t, resp), &env); err != nil {
		t.Fatalf("envelope: %v (body %q)", err, bodyBytes(t, resp))
	}
	return env
}

func sessionCookie(resp *http.Response) string {
	for _, c := range resp.Cookies() {
		if c.Name == "post_session" {
			return c.Value
		}
	}
	return ""
}

const (
	signupBody  = `{"email":"e2e-alice@example.com","password":"long-enough-password-1"}`
	signupBody2 = `{"email":"e2e-bob@example.com","password":"long-enough-password-2"}`
	loginBody   = `{"email":"e2e-alice@example.com","password":"long-enough-password-1"}`
)

// TestE2EUnauthenticatedWrite401 is acceptance "未认证写 API 401" over the
// wire: anonymous state changes answer 401 with the stable envelope — even
// for product routes that do not exist yet (the guard runs before routing).
func TestE2EUnauthenticatedWrite401(t *testing.T) {
	env := newE2EEnv(t, defaultCfg(), nil)

	cases := []struct{ method, path string }{
		{http.MethodPost, "/api/v1/projects"},
		{http.MethodPut, "/api/v1/projects/1"},
		{http.MethodDelete, "/api/v1/projects/1"},
		{http.MethodPost, "/api/v1/auth/logout"},
	}
	for _, tc := range cases {
		resp := env.do(t, tc.method, tc.path, "{}", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s anonymous = %d, want 401", tc.method, tc.path, resp.StatusCode)
			continue
		}
		if env := decodeEnvelope(t, resp); env.Code != authn.CodeUnauthenticated {
			t.Errorf("%s %s code = %q, want %q", tc.method, tc.path, env.Code, authn.CodeUnauthenticated)
		}
		if sessionCookie(resp) != "" {
			t.Errorf("%s %s set a session cookie on a rejected write", tc.method, tc.path)
		}
	}
}

// TestE2ELoginLogoutJourney is acceptance "登录/退出 E2E": signup issues a
// server-side session, whoami resolves it, CSRF-bound logout revokes it in
// Redis, and the same cookie stops authenticating everywhere.
func TestE2ELoginLogoutJourney(t *testing.T) {
	env := newE2EEnv(t, defaultCfg(), nil)

	// Signup.
	resp := env.do(t, http.MethodPost, "/api/v1/auth/signup", signupBody, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("signup = %d: %s", resp.StatusCode, bodyBytes(t, resp))
	}
	token := sessionCookie(resp)
	if token == "" {
		t.Fatal("signup did not set post_session")
	}
	var payload struct {
		User struct {
			Email string `json:"email"`
		} `json:"user"`
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(bodyBytes(t, resp), &payload); err != nil {
		t.Fatalf("signup body: %v", err)
	}
	if payload.User.Email != "e2e-alice@example.com" || payload.CSRFToken == "" {
		t.Fatalf("signup payload = %+v", payload)
	}

	// Whoami.
	resp = env.do(t, http.MethodGet, "/api/v1/auth/session", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("whoami = %d: %s", resp.StatusCode, bodyBytes(t, resp))
	}

	// Logout (CSRF-bound), then the cookie is dead.
	resp = env.do(t, http.MethodPost, "/api/v1/auth/logout", "",
		map[string]string{"X-CSRF-Token": payload.CSRFToken})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout = %d: %s", resp.StatusCode, bodyBytes(t, resp))
	}
	resp = env.do(t, http.MethodGet, "/api/v1/auth/session", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("whoami after logout = %d, want 401", resp.StatusCode)
	}

	// Login issues a fresh session.
	resp = env.do(t, http.MethodPost, "/api/v1/auth/login", loginBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d: %s", resp.StatusCode, bodyBytes(t, resp))
	}
	if got := sessionCookie(resp); got == "" || got == token {
		t.Error("login must issue a fresh session cookie")
	}
	resp = env.do(t, http.MethodGet, "/api/v1/auth/session", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("whoami after login = %d, want 200", resp.StatusCode)
	}
}

// TestE2EEnumerationIdentical is acceptance "账号枚举防护" over the wire:
// wrong password and unknown email produce indistinguishable 401s — same
// status, code, message, retryability, and neither sets a cookie.
func TestE2EEnumerationIdentical(t *testing.T) {
	env := newE2EEnv(t, defaultCfg(), nil)
	if resp := env.do(t, http.MethodPost, "/api/v1/auth/signup", signupBody, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed signup = %d", resp.StatusCode)
	}

	wrongPass := env.do(t, http.MethodPost, "/api/v1/auth/login",
		`{"email":"e2e-alice@example.com","password":"wrong-password-456"}`, nil)
	unknownEmail := env.do(t, http.MethodPost, "/api/v1/auth/login",
		`{"email":"nobody@example.com","password":"wrong-password-456"}`, nil)

	if wrongPass.StatusCode != http.StatusUnauthorized || unknownEmail.StatusCode != http.StatusUnauthorized {
		t.Fatalf("statuses = %d / %d, want 401 / 401", wrongPass.StatusCode, unknownEmail.StatusCode)
	}
	a, b := decodeEnvelope(t, wrongPass), decodeEnvelope(t, unknownEmail)
	if a.Code != b.Code || a.Message != b.Message || a.Retryable != b.Retryable {
		t.Errorf("envelopes differ: %+v vs %+v — enumeration leak", a, b)
	}
	if a.Code != authn.CodeInvalidCredentials {
		t.Errorf("code = %q, want %q", a.Code, authn.CodeInvalidCredentials)
	}
	if sessionCookie(wrongPass) != "" || sessionCookie(unknownEmail) != "" {
		t.Error("failed logins must not set a session cookie")
	}
}

// TestE2ECSRFAndRateLimit exercises the two other basics through real
// Redis: a session without X-CSRF-Token gets 403; after the per-email
// window, even the correct password gets 429 + Retry-After (the limiter
// counts in Redis).
func TestE2ECSRFAndRateLimit(t *testing.T) {
	cfg := defaultCfg()
	cfg.LoginLimitPerEmail = 3
	env := newE2EEnv(t, cfg, nil)

	resp := env.do(t, http.MethodPost, "/api/v1/auth/signup", signupBody, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("signup = %d", resp.StatusCode)
	}
	// The cookiejar holds the session now; a write without the CSRF header
	// must be 403 even though the session is valid.
	resp = env.do(t, http.MethodPost, "/api/v1/projects", "{}", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("authenticated write without CSRF = %d, want 403", resp.StatusCode)
	}
	if env := decodeEnvelope(t, resp); env.Code != authn.CodeCSRFFailed {
		t.Errorf("code = %q, want %q", env.Code, authn.CodeCSRFFailed)
	}

	// Wrong CSRF: same 403.
	resp = env.do(t, http.MethodPost, "/api/v1/projects", "{}",
		map[string]string{"X-CSRF-Token": "forged"})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("write with forged CSRF = %d, want 403", resp.StatusCode)
	}

	// Rate limit: exhaust the email window with wrong passwords.
	for i := 0; i < 3; i++ {
		resp := env.do(t, http.MethodPost, "/api/v1/auth/login",
			`{"email":"e2e-alice@example.com","password":"wrong-password-999"}`, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i+1, resp.StatusCode)
		}
	}
	// The 4th attempt — with the CORRECT password — is refused by the
	// limiter (limit is on attempts, not failures: no oracle).
	resp = env.do(t, http.MethodPost, "/api/v1/auth/login", loginBody, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("4th attempt = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("429 must carry Retry-After")
	}
	if env := decodeEnvelope(t, resp); env.Code != authn.CodeRateLimited {
		t.Errorf("code = %q, want %q", env.Code, authn.CodeRateLimited)
	}
}

// TestE2EOIDCFlow drives the full OIDC journey against a real IdP on the
// wire: authorize-url (state cookie) → provider redirect → callback (state
// bound to the browser, code exchanged, RS256 verified) → session in Redis
// → whoami shows the OIDC identity.
func TestE2EOIDCFlow(t *testing.T) {
	provider := oidctest.NewProvider()
	provider.Serve()
	defer provider.Close()

	cfg := defaultCfg()
	cfg.OIDC = authn.OIDCConfig{
		Enabled:      true,
		Issuer:       provider.Issuer(),
		ClientID:     oidctest.ClientID,
		RedirectPath: "/api/v1/auth/oidc/callback",
	}
	env := newE2EEnv(t, cfg, provider.NewClient(""))

	// Step 1: the web app asks for the authorize URL; the API binds a
	// fresh state to this browser via HttpOnly cookie.
	resp := env.do(t, http.MethodGet, "/api/v1/auth/oidc/authorize-url", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorize-url = %d: %s", resp.StatusCode, bodyBytes(t, resp))
	}
	state := ""
	for _, c := range resp.Cookies() {
		if c.Name == "post_oidc_state" {
			state = c.Value
		}
	}
	if state == "" {
		t.Fatal("no post_oidc_state cookie")
	}
	var body map[string]string
	if err := json.Unmarshal(bodyBytes(t, resp), &body); err != nil {
		t.Fatalf("authorize-url body: %v", err)
	}
	if !strings.HasPrefix(body["authorize_url"], provider.Issuer()+"/authorize?") {
		t.Errorf("authorize_url = %q", body["authorize_url"])
	}
	// The authorize URL must carry this API's own callback endpoint as
	// redirect_uri (request-derived, not a startup constant).
	parsed, err := url.Parse(body["authorize_url"])
	if err != nil {
		t.Fatalf("authorize_url parse: %v", err)
	}
	if redirect := parsed.Query().Get("redirect_uri"); !strings.HasPrefix(redirect, "http://"+env.api.Listener.Addr().String()) ||
		!strings.HasSuffix(redirect, "/api/v1/auth/oidc/callback") {
		t.Errorf("redirect_uri = %q, want this API's own callback endpoint", redirect)
	}

	// Step 2: the browser "returns" from the IdP with code+state. (The
	// cookiejar carries the state cookie automatically.)
	code := provider.IssueCode(oidctest.Person{
		Subject: "sub-e2e-oidc", Email: "oidc.e2e@example.com", EmailVerified: true,
		PreferredUsername: "oidc-e2e", Name: "OIDC E2E",
	})
	resp = env.do(t, http.MethodGet, "/api/v1/auth/oidc/callback?code="+code+"&state="+state, "", nil)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback = %d: %s", resp.StatusCode, bodyBytes(t, resp))
	}
	if loc := resp.Header.Get("Location"); loc != cfg.WebOrigin+"/" {
		t.Errorf("callback Location = %q, want %q", loc, cfg.WebOrigin+"/")
	}
	if sessionCookie(resp) == "" {
		t.Fatal("callback did not issue a session")
	}

	// Step 3: the session resolves to the OIDC identity.
	resp = env.do(t, http.MethodGet, "/api/v1/auth/session", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("OIDC whoami = %d", resp.StatusCode)
	}
	var payload struct {
		User struct {
			Email  string `json:"email"`
			Handle string `json:"handle"`
		} `json:"user"`
	}
	if err := json.Unmarshal(bodyBytes(t, resp), &payload); err != nil {
		t.Fatalf("whoami body: %v", err)
	}
	if payload.User.Email != "oidc.e2e@example.com" || payload.User.Handle != "oidc-e2e" {
		t.Errorf("OIDC whoami = %+v", payload.User)
	}
}

// TestE2ECrossOriginWebLoginJourney is the B1 regression over the wire: the
// shipped web app calls the API cross-origin (web http://web.test, API on
// 127.0.0.1:<port>), and a browser sends the Origin header on every
// cross-origin POST. The pre-auth endpoints (login/signup) must accept the
// configured WebOrigin — the earlier guard only ever compared the Origin
// host against r.Host, so this journey answered 403 and the delivered login
// page could not authenticate against the delivered API. Session-bearing
// writes (logout) stay CSRF-protected.
func TestE2ECrossOriginWebLoginJourney(t *testing.T) {
	env := newE2EEnv(t, defaultCfg(), nil)
	webHeaders := map[string]string{"Origin": defaultCfg().WebOrigin}

	// Signup from the web origin (cross-origin to the API host).
	resp := env.do(t, http.MethodPost, "/api/v1/auth/signup", signupBody, webHeaders)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("cross-origin signup = %d: %s", resp.StatusCode, bodyBytes(t, resp))
	}
	var payload struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(bodyBytes(t, resp), &payload); err != nil || payload.CSRFToken == "" {
		t.Fatalf("signup payload = %s (%v)", bodyBytes(t, resp), err)
	}

	// Whoami (read) resolves the session.
	resp = env.do(t, http.MethodGet, "/api/v1/auth/session", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("whoami = %d: %s", resp.StatusCode, bodyBytes(t, resp))
	}

	// Logout: a session-bearing write protected by the CSRF token — a
	// browser sends Origin here too, and the guard must not 403 it (the
	// Origin relaxation is only for the pre-session endpoints).
	resp = env.do(t, http.MethodPost, "/api/v1/auth/logout", "",
		map[string]string{"Origin": defaultCfg().WebOrigin, "X-CSRF-Token": payload.CSRFToken})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("cross-origin logout = %d: %s", resp.StatusCode, bodyBytes(t, resp))
	}

	// Login from the web origin mints a fresh session.
	resp = env.do(t, http.MethodPost, "/api/v1/auth/login", loginBody, webHeaders)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cross-origin login = %d: %s", resp.StatusCode, bodyBytes(t, resp))
	}
	if sessionCookie(resp) == "" {
		t.Fatal("cross-origin login did not set post_session")
	}

	// A foreign origin is still refused on the pre-auth endpoints.
	resp = env.do(t, http.MethodPost, "/api/v1/auth/login", loginBody,
		map[string]string{"Origin": "https://evil.example"})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("login with foreign Origin = %d, want 403", resp.StatusCode)
	}
	if env := decodeEnvelope(t, resp); env.Code != authn.CodeCSRFFailed {
		t.Errorf("foreign-origin code = %q, want %q", env.Code, authn.CodeCSRFFailed)
	}
}

// TestE2ESignupRateLimit: signup burns a full argon2id KDF plus an INSERT
// per anonymous call, so it is rate-limited per IP through real Redis —
// the M2 regression (before the fix every request went straight to the
// KDF and the database).
func TestE2ESignupRateLimit(t *testing.T) {
	cfg := defaultCfg()
	cfg.SignupLimitPerIP = 2
	env := newE2EEnv(t, cfg, nil)

	// Two signups from this IP are within budget.
	for i := 0; i < 2; i++ {
		body := fmt.Sprintf(`{"email":"e2e-s%d@example.com","password":"long-enough-password-%d"}`, i, i)
		resp := env.do(t, http.MethodPost, "/api/v1/auth/signup", body, nil)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("signup %d = %d: %s", i+1, resp.StatusCode, bodyBytes(t, resp))
		}
	}
	// The third — a perfectly valid request — is refused by the limiter.
	resp := env.do(t, http.MethodPost, "/api/v1/auth/signup",
		`{"email":"e2e-s3@example.com","password":"long-enough-password-3"}`, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("3rd signup = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("signup 429 must carry Retry-After")
	}
	if env := decodeEnvelope(t, resp); env.Code != authn.CodeRateLimited {
		t.Errorf("signup 429 code = %q, want %q", env.Code, authn.CodeRateLimited)
	}
}

// TestE2ESessionIsServerSide: sessions live in Redis, not in the API
// process — a second API instance sharing the same Redis accepts the same
// cookie. This is the property that makes logout/hijack-revocation real.
func TestE2ESessionIsServerSide(t *testing.T) {
	env := newE2EEnv(t, defaultCfg(), nil)

	// Second API instance over the SAME Redis: same adapters, new process
	// boundary (a fresh authhttp.API + server).
	second := authhttp.New(authhttp.Deps{
		Users:      env.users,
		Sessions:   persistence.NewRedisSessionStore(redis.NewClient(&redis.Options{Addr: env.redis.Addr()})),
		Limiter:    persistence.NewRedisRateLimiter(redis.NewClient(&redis.Options{Addr: env.redis.Addr()})),
		OIDCClient: nil,
		Cfg:        defaultCfg(),
		Secure:     false,
	})
	secondMux := http.NewServeMux()
	secondMux.Handle("/api/v1/auth/", second.Routes())
	ts2 := httptest.NewServer(second.Guard(secondMux))
	defer ts2.Close()

	resp := env.do(t, http.MethodPost, "/api/v1/auth/signup", signupBody2, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("signup = %d", resp.StatusCode)
	}
	token := sessionCookie(resp)

	// Whoami against the SECOND server with the first server's cookie.
	req, err := http.NewRequest(http.MethodGet, ts2.URL+"/api/v1/auth/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: "post_session", Value: token})
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("whoami on second instance: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("second instance whoami = %d, want 200 (session must be server-side)", resp2.StatusCode)
	}
}
