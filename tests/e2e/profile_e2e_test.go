package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/profilehttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/profile"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
)

// The profile e2e suite runs the T0102 surface the way a browser/API
// client does: real HTTP servers and clients, the production guard +
// handlers (cmd/api/authhttp + cmd/api/profilehttp), real Redis protocol
// (miniredis) and one in-memory identity space (memstore implements both
// the authn.UserStore and the profile.ProfileStore ports — the production
// graph over PostgreSQL is covered by tests/integration).
//
// T0102-TEST-02 "profile e2e" (blocking): go test ./tests/e2e -run TestE2EProfile

// newProfileE2EEnv mirrors cmd/api main.go's product wiring: one guarded
// /api/v1 mux carrying the auth surface and the profile surface — the
// exact composition the production binary mounts.
func newProfileE2EEnv(t *testing.T, cfg authn.Config) *e2eEnv {
	t.Helper()
	users := memstore.NewUsers()
	mr := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	authAPI := authhttp.New(authhttp.Deps{
		Users:      users,
		Sessions:   persistence.NewRedisSessionStore(redisClient),
		Limiter:    persistence.NewRedisRateLimiter(redisClient),
		OIDCClient: nil,
		Cfg:        cfg,
		Secure:     false,
	})
	profileAPI := profilehttp.New(profilehttp.Deps{Profiles: users})
	v1 := http.NewServeMux()
	authAPI.Register(v1)
	profileAPI.Register(v1)
	ts := httptest.NewServer(authAPI.Guard(v1))
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

// e2eSignupPayload is the wire shape both signup and login answer.
type e2eSignupPayload struct {
	User struct {
		ID          string `json:"id"`
		Handle      string `json:"handle"`
		DisplayName string `json:"display_name"`
		Email       string `json:"email"`
	} `json:"user"`
	CSRFToken string `json:"csrf_token"`
}

// e2eSignup drives a full signup over the wire and returns the user id +
// CSRF token the caller must echo. The env's cookiejar is the "browser":
// after this call the jar carries the new session cookie.
func e2eSignup(t *testing.T, env *e2eEnv, body string) e2eSignupPayload {
	t.Helper()
	resp := env.do(t, http.MethodPost, "/api/v1/auth/signup", body, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("signup = %d: %s", resp.StatusCode, bodyBytes(t, resp))
	}
	var payload e2eSignupPayload
	if err := json.Unmarshal(bodyBytes(t, resp), &payload); err != nil {
		t.Fatalf("signup body: %v", err)
	}
	if payload.CSRFToken == "" {
		t.Fatal("signup did not return a CSRF token")
	}
	return payload
}

type e2eProfilePayload struct {
	ID          string `json:"id"`
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
	Bio         string `json:"bio"`
	CreatedAt   string `json:"created_at"`
}

// e2eGetProfile GETs a public profile and decodes it; anything but 200 is a
// test failure (the callers assert 404s themselves via env.do).
func e2eGetProfile(t *testing.T, env *e2eEnv, path string) e2eProfilePayload {
	t.Helper()
	resp := env.do(t, http.MethodGet, path, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, resp.StatusCode, bodyBytes(t, resp))
	}
	var p e2eProfilePayload
	if err := json.Unmarshal(bodyBytes(t, resp), &p); err != nil {
		t.Fatalf("profile body: %v", err)
	}
	return p
}

func freshJar() http.CookieJar {
	jar, _ := cookiejar.New(nil)
	return jar
}

// TestE2EProfileJourney is the acceptance journey "未登录可读公开 profile"
// + "用户只能改自己可编辑字段" over the wire: signup → anonymous public
// read (email absent) → owner edit through the CSRF guard → fields land →
// non-owner edit refused → anonymous edit refused → the id-keyed URL stays
// stable across a handle rename.
func TestE2EProfileJourney(t *testing.T) {
	env := newProfileE2EEnv(t, defaultCfg())

	// Alice signs up through the real API. The env's client jar is her
	// browser: it now carries her session cookie. Keep the jar reference —
	// the journey swaps to other browsers and must come back.
	aliceJar := env.client.Jar
	alice := e2eSignup(t, env, `{"email":"e2e-alice@example.com","password":"long-enough-password-1","handle":"alice","display_name":"Alice"}`)

	// --- Anonymous public read -------------------------------------------
	// A fresh cookiejar simulates a signed-out browser: GET must succeed
	// and carry exactly the public fields.
	env.client.Jar = freshJar()
	pub := e2eGetProfile(t, env, "/api/v1/users/"+alice.User.ID+"/profile")
	if pub.Handle != "alice" || pub.DisplayName != "Alice" || pub.Bio != "" {
		t.Errorf("anonymous profile = %+v", pub)
	}
	// Email must not appear anywhere in the anonymous response body — the
	// structural privacy check, not a field-value check.
	resp := env.do(t, http.MethodGet, "/api/v1/users/"+alice.User.ID+"/profile", "", nil)
	if raw := string(bodyBytes(t, resp)); strings.Contains(raw, "e2e-alice@example.com") || strings.Contains(raw, `"email"`) {
		t.Errorf("anonymous public profile leaked email: %s", raw)
	}

	// --- Owner edit (back on Alice's browser) -----------------------------
	env.client.Jar = aliceJar
	authHeaders := map[string]string{"X-CSRF-Token": alice.CSRFToken}
	resp = env.do(t, http.MethodPatch, "/api/v1/users/"+alice.User.ID+"/profile",
		`{"display_name":"Alice Researcher","bio":"alloy design & ML potentials","handle":"alice-r"}`,
		authHeaders)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owner PATCH = %d: %s", resp.StatusCode, bodyBytes(t, resp))
	}
	var updated e2eProfilePayload
	if err := json.Unmarshal(bodyBytes(t, resp), &updated); err != nil {
		t.Fatalf("PATCH body: %v", err)
	}
	if updated.DisplayName != "Alice Researcher" || updated.Bio != "alloy design & ML potentials" || updated.Handle != "alice-r" {
		t.Errorf("updated profile = %+v", updated)
	}

	// --- Stable profile URL across the rename -----------------------------
	// The id-keyed URL still answers; the handle lookup follows the rename;
	// the old handle no longer resolves.
	byID := e2eGetProfile(t, env, "/api/v1/users/"+alice.User.ID+"/profile")
	if byID.Handle != "alice-r" {
		t.Errorf("id-keyed profile after rename = %+v", byID)
	}
	if byNew := e2eGetProfile(t, env, "/api/v1/users/by-handle/alice-r/profile"); byNew.ID != alice.User.ID {
		t.Errorf("by-handle(new) = %+v", byNew)
	}
	oldResp := env.do(t, http.MethodGet, "/api/v1/users/by-handle/alice/profile", "", nil)
	if oldResp.StatusCode != http.StatusNotFound {
		t.Errorf("by-handle(old) = %d, want 404", oldResp.StatusCode)
	}

	// --- Non-owner edit refused -------------------------------------------
	// Bob signs up on a second browser and tries to edit Alice's profile:
	// 403 AUTH_FORBIDDEN, and the profile is untouched.
	env.client.Jar = freshJar()
	bob := e2eSignup(t, env, `{"email":"e2e-bob@example.com","password":"long-enough-password-2","handle":"bob","display_name":"Bob"}`)
	resp = env.do(t, http.MethodPatch, "/api/v1/users/"+alice.User.ID+"/profile",
		`{"display_name":"Bob the Usurper"}`,
		map[string]string{"X-CSRF-Token": bob.CSRFToken})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign PATCH = %d, want 403", resp.StatusCode)
	}
	if envl := decodeEnvelope(t, resp); envl.Code != profile.CodeForbidden {
		t.Errorf("foreign PATCH code = %q, want %q", envl.Code, profile.CodeForbidden)
	}

	// --- Anonymous edit refused -------------------------------------------
	env.client.Jar = freshJar()
	resp = env.do(t, http.MethodPatch, "/api/v1/users/"+alice.User.ID+"/profile",
		`{"display_name":"Anonymous"}`, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous PATCH = %d, want 401", resp.StatusCode)
	}
	if envl := decodeEnvelope(t, resp); envl.Code != authn.CodeUnauthenticated {
		t.Errorf("anonymous PATCH code = %q, want %q", envl.Code, authn.CodeUnauthenticated)
	}

	// The profile still shows Alice's own edits — the refused attempts
	// changed nothing.
	if final := e2eGetProfile(t, env, "/api/v1/users/"+alice.User.ID+"/profile"); final.DisplayName != "Alice Researcher" {
		t.Errorf("profile after refused edits = %+v", final)
	}
}

// TestE2EProfileDisabledAccount: a disabled account cannot authenticate
// (the guard reloads the user on every request and drops disabled
// sessions), but its public profile stays readable — nothing disappears
// (CLAUDE.md §9.8).
func TestE2EProfileDisabledAccount(t *testing.T) {
	env := newProfileE2EEnv(t, defaultCfg())
	carolJar := env.client.Jar
	carol := e2eSignup(t, env, `{"email":"e2e-carol@example.com","password":"long-enough-password-3","handle":"carol","display_name":"Carol"}`)

	// Disable the account by rewriting the stored record (simulates an
	// admin disable; the auth surface has no disable endpoint in V1).
	rec, err := env.users.GetByID(t.Context(), carol.User.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	disabledAt := time.Now().UTC()
	rec.User.DisabledAt = &disabledAt
	env.users.Seed(rec) // Seed overwrites the store with the disabled record

	// Signed-out read still works.
	env.client.Jar = freshJar()
	resp := env.do(t, http.MethodGet, "/api/v1/users/"+carol.User.ID+"/profile", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("disabled account profile = %d, want 200 (nothing disappears)", resp.StatusCode)
	}

	// The pre-disable session cookie no longer authenticates: the guard
	// re-authenticates per request and the disabled session is 401 on
	// writes — even with the session's own CSRF token.
	env.client.Jar = carolJar
	resp = env.do(t, http.MethodPatch, "/api/v1/users/"+carol.User.ID+"/profile",
		`{"bio":"reactivated?"}`, map[string]string{"X-CSRF-Token": carol.CSRFToken})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("disabled session PATCH = %d, want 401", resp.StatusCode)
	}
}

// TestE2EProfileValidationAndConflict: field validation over the wire —
// bad handle/display/bio are 400 VALIDATION_FAILED, a taken handle is 409
// HANDLE_ALREADY_TAKEN, and the email field cannot be patched (unknown
// fields are ignored).
func TestE2EProfileValidationAndConflict(t *testing.T) {
	env := newProfileE2EEnv(t, defaultCfg())
	dan := e2eSignup(t, env, `{"email":"e2e-dan@example.com","password":"long-enough-password-4","handle":"dan","display_name":"Dan"}`)
	// Eve's signup replaces the jar's session; re-login as Dan below so
	// the PATCHes act as Dan, not Eve.
	e2eSignup(t, env, `{"email":"e2e-eve@example.com","password":"long-enough-password-5","handle":"eve","display_name":"Eve"}`)

	resp := env.do(t, http.MethodPost, "/api/v1/auth/login",
		`{"email":"e2e-dan@example.com","password":"long-enough-password-4"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("re-login dan = %d", resp.StatusCode)
	}
	var loginPayload e2eSignupPayload
	if err := json.Unmarshal(bodyBytes(t, resp), &loginPayload); err != nil {
		t.Fatalf("login body: %v", err)
	}
	authHeaders := map[string]string{"X-CSRF-Token": loginPayload.CSRFToken}

	cases := []struct {
		body string
		want int
		code string
	}{
		{`{"handle":"has space!"}`, http.StatusBadRequest, profile.CodeValidation},
		{`{"handle":"eve"}`, http.StatusConflict, profile.CodeHandleTaken},
		{`{"display_name":""}`, http.StatusBadRequest, profile.CodeValidation},
		{`{"display_name":"   "}`, http.StatusBadRequest, profile.CodeValidation},
		{`{"bio":"` + strings.Repeat("b", 2001) + `"}`, http.StatusBadRequest, profile.CodeValidation},
	}
	for _, tc := range cases {
		resp := env.do(t, http.MethodPatch, "/api/v1/users/"+dan.User.ID+"/profile", tc.body, authHeaders)
		if resp.StatusCode != tc.want {
			t.Errorf("PATCH %s = %d, want %d", tc.body, resp.StatusCode, tc.want)
			continue
		}
		if envl := decodeEnvelope(t, resp); envl.Code != tc.code {
			t.Errorf("PATCH %s code = %q, want %q", tc.body, envl.Code, tc.code)
		}
	}

	// Email is not an editable field: patching it is ignored (the profile
	// keeps Dan's email — nothing in the response carries any email).
	resp = env.do(t, http.MethodPatch, "/api/v1/users/"+dan.User.ID+"/profile",
		`{"email":"attacker@example.com","display_name":"Dan P."}`, authHeaders)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("email-field PATCH = %d", resp.StatusCode)
	}
	rawBody := bodyBytes(t, resp)
	if strings.Contains(string(rawBody), "@example.com") {
		t.Errorf("PATCH response leaked an email: %s", rawBody)
	}
	var p e2eProfilePayload
	if err := json.Unmarshal(rawBody, &p); err != nil {
		t.Fatalf("PATCH body: %v", err)
	}
	if p.DisplayName != "Dan P." {
		t.Errorf("display_name = %q, want Dan P.", p.DisplayName)
	}
}
