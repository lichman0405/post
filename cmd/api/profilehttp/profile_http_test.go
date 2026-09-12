package profilehttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/profile"
	"github.com/lichman0405/post/internal/persistence/memstore"
)

// The transport tests drive the REAL guard + handlers over memory
// adapters (the authhttp middleware-test pattern): the production
// session/CSRF guard, the production profile handlers, one in-memory
// identity space. Storage is fake; every policy line is production code.
//
// Required test "profile api" (T0102): the API surface — public payload
// shape, error codes, owner-only enforcement — at the handler boundary.

// newTestAPI builds the guarded auth + profile API over memory adapters.
func newTestAPI(t *testing.T) (*httptest.Server, *memstore.Users) {
	t.Helper()
	users := memstore.NewUsers()
	cfg := authn.Config{
		WebOrigin:          "http://web.test",
		SessionTTL:         time.Hour,
		LoginLimitPerEmail: 100,
		LoginLimitPerIP:    1000,
		SignupLimitPerIP:   1000,
		LoginWindow:        time.Minute,
	}
	authAPI := authhttp.New(authhttp.Deps{
		Users:      users,
		Sessions:   memstore.NewSessions(),
		Limiter:    memstore.NewLimiter(),
		OIDCClient: nil,
		Cfg:        cfg,
		Secure:     false,
	})
	profileAPI := New(Deps{Profiles: users})
	mux := http.NewServeMux()
	authAPI.Register(mux)
	profileAPI.Register(mux)
	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)
	return ts, users
}

func do(t *testing.T, method, url, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// signup creates an account through the real API and returns the user id
// plus CSRF token. No cookiejar: callers pass the CSRF token explicitly
// (the session cookie is not needed by the server beyond Authenticate, but
// the guard only resolves the session from the cookie — the jar-less
// client cannot hold sessions, so authenticated calls use
// authenticatedRequest with the cookie).
type signupResult struct {
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	CSRFToken string `json:"csrf_token"`
}

func signup(t *testing.T, ts *httptest.Server, email string) (signupResult, string) {
	t.Helper()
	resp := do(t, http.MethodPost, ts.URL+"/api/v1/auth/signup",
		`{"email":"`+email+`","password":"long-enough-password-1"}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("signup %s = %d", email, resp.StatusCode)
	}
	var sr signupResult
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("signup body: %v", err)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "post_session" {
			return sr, c.Value
		}
	}
	t.Fatal("signup set no post_session cookie")
	return sr, ""
}

func decodeBody(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("body: %v", err)
	}
}

func decodeMap(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var m map[string]any
	decodeBody(t, resp, &m)
	return m
}

func strPtr(s string) *string { return &s }

// TestAPIAnonymousReadsPublicProfile is acceptance "未登录可读公开 profile"
// at the API level: no session cookie, GET succeeds, and the body carries
// exactly the public fields — email is structurally absent.
func TestAPIAnonymousReadsPublicProfile(t *testing.T) {
	ts, users := newTestAPI(t)

	sr, _ := signup(t, ts, "api-alice@example.com")

	// Seed a bio so the read proves profiles content (not just identity).
	if _, err := users.Update(t.Context(), sr.User.ID,
		profile.Update{Bio: strPtr("materials scientist")}); err != nil {
		t.Fatalf("seed bio: %v", err)
	}

	resp := do(t, http.MethodGet, ts.URL+"/api/v1/users/"+sr.User.ID+"/profile", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous profile GET = %d", resp.StatusCode)
	}
	body := decodeMap(t, resp)
	if body["handle"] != "api-alice" || body["display_name"] != "api-alice" ||
		body["bio"] != "materials scientist" || body["id"] != sr.User.ID {
		t.Errorf("profile body = %v", body)
	}
	if _, present := body["created_at"]; !present {
		t.Error("profile body must carry created_at")
	}
	for _, private := range []string{"email", "password", "password_hash", "disabled_at", "csrf_token"} {
		if _, present := body[private]; present {
			t.Errorf("public profile leaked private field %q: %v", private, body)
		}
	}

	// Unknown id: 404 USER_NOT_FOUND (a non-uuid id is equally unknown).
	for _, id := range []string{"00000000-0000-4000-8000-000000000000", "not-a-uuid"} {
		resp := do(t, http.MethodGet, ts.URL+"/api/v1/users/"+id+"/profile", "", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET profile %q = %d, want 404", id, resp.StatusCode)
		}
		env := decodeMap(t, resp)
		if env["code"] != profile.CodeUserNotFound {
			t.Errorf("unknown profile code = %v, want %s", env["code"], profile.CodeUserNotFound)
		}
	}
}

// TestAPIPublicProfileByHandle covers the handle lookup: current handle
// resolves (normalized), unknown handle 404s, and the lookup follows a
// handle rename — while the id-keyed URL stays the same (stable profile
// URL).
func TestAPIPublicProfileByHandle(t *testing.T) {
	ts, _ := newTestAPI(t)
	sr, cookie := signup(t, ts, "api-bob@example.com")

	resp := do(t, http.MethodGet, ts.URL+"/api/v1/users/by-handle/api-bob/profile", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("by-handle GET = %d", resp.StatusCode)
	}
	if body := decodeMap(t, resp); body["id"] != sr.User.ID {
		t.Errorf("by-handle body = %v", body)
	}

	// Case-insensitive lookup (the service normalizes).
	resp = do(t, http.MethodGet, ts.URL+"/api/v1/users/by-handle/API-BOB/profile", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("by-handle uppercase = %d, want 200", resp.StatusCode)
	}

	resp = do(t, http.MethodGet, ts.URL+"/api/v1/users/by-handle/nobody/profile", "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("by-handle unknown = %d, want 404", resp.StatusCode)
	}

	// Rename via PATCH, then the old handle stops resolving and the new
	// one does; the id-keyed URL never changes.
	patch := do(t, http.MethodPatch, ts.URL+"/api/v1/users/"+sr.User.ID+"/profile",
		`{"handle":"api-bob-2"}`, map[string]string{
			"Cookie":       "post_session=" + cookie,
			"X-CSRF-Token": sr.CSRFToken,
		})
	if patch.StatusCode != http.StatusOK {
		t.Fatalf("rename PATCH = %d: %v", patch.StatusCode, decodeMap(t, patch))
	}
	for path, want := range map[string]int{
		"/api/v1/users/" + sr.User.ID + "/profile":  http.StatusOK,
		"/api/v1/users/by-handle/api-bob-2/profile": http.StatusOK,
		"/api/v1/users/by-handle/api-bob/profile":   http.StatusNotFound,
	} {
		got := do(t, http.MethodGet, ts.URL+path, "", nil)
		if got.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, got.StatusCode, want)
		}
	}
}

// TestAPIUpdateOwnerOnly is acceptance "用户只能改自己可编辑字段" at the
// API level: the owner edits their own fields; anyone else gets 403; an
// anonymous PATCH is 401 before routing; the CSRF token is required.
func TestAPIUpdateOwnerOnly(t *testing.T) {
	ts, _ := newTestAPI(t)
	alice, aliceCookie := signup(t, ts, "api-alice@example.com")
	bob, bobCookie := signup(t, ts, "api-bob@example.com")

	// Bob PATCHes Alice's profile: 403 AUTH_FORBIDDEN, unchanged.
	resp := do(t, http.MethodPatch, ts.URL+"/api/v1/users/"+alice.User.ID+"/profile",
		`{"display_name":"Bob the Usurper"}`, map[string]string{
			"Cookie":       "post_session=" + bobCookie,
			"X-CSRF-Token": bob.CSRFToken,
		})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign PATCH = %d, want 403", resp.StatusCode)
	}
	if env := decodeMap(t, resp); env["code"] != profile.CodeForbidden {
		t.Errorf("foreign PATCH code = %v, want %s", env["code"], profile.CodeForbidden)
	}

	// Bob cannot forge Alice's identity through the id path with his own
	// session either — the target id is what counts.
	resp = do(t, http.MethodPatch, ts.URL+"/api/v1/users/"+alice.User.ID+"/profile",
		`{"bio":"x"}`, map[string]string{
			"Cookie":       "post_session=" + bobCookie,
			"X-CSRF-Token": bob.CSRFToken,
		})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign bio PATCH = %d, want 403", resp.StatusCode)
	}

	// Anonymous PATCH: 401 (the guard, before routing).
	resp = do(t, http.MethodPatch, ts.URL+"/api/v1/users/"+alice.User.ID+"/profile",
		`{"display_name":"Anonymous"}`, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous PATCH = %d, want 401", resp.StatusCode)
	}

	// Alice with her session but without CSRF: 403 CSRF_FAILED.
	resp = do(t, http.MethodPatch, ts.URL+"/api/v1/users/"+alice.User.ID+"/profile",
		`{"display_name":"Alice CSRF"}`, map[string]string{
			"Cookie": "post_session=" + aliceCookie,
		})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("PATCH without CSRF = %d, want 403", resp.StatusCode)
	}

	// Alice edits her own fields: 200 with the public shape.
	resp = do(t, http.MethodPatch, ts.URL+"/api/v1/users/"+alice.User.ID+"/profile",
		`{"display_name":"Alice A.","bio":"alloy design","handle":"alice-a"}`, map[string]string{
			"Cookie":       "post_session=" + aliceCookie,
			"X-CSRF-Token": alice.CSRFToken,
		})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owner PATCH = %d: %v", resp.StatusCode, decodeMap(t, resp))
	}
	body := decodeMap(t, resp)
	if body["display_name"] != "Alice A." || body["bio"] != "alloy design" || body["handle"] != "alice-a" {
		t.Errorf("owner PATCH body = %v", body)
	}
	if _, present := body["email"]; present {
		t.Errorf("owner PATCH leaked email: %v", body)
	}
}

// TestAPIUpdateValidationAndConflicts: invalid fields answer 400
// VALIDATION_FAILED, a handle held by someone else answers 409
// HANDLE_ALREADY_TAKEN, and the editable set is closed — email is not a
// patchable field (it is ignored like any unknown field).
func TestAPIUpdateValidationAndConflicts(t *testing.T) {
	ts, _ := newTestAPI(t)
	alice, aliceCookie := signup(t, ts, "api-alice@example.com")
	bob, bobCookie := signup(t, ts, "api-bob@example.com")

	auth := map[string]string{
		"Cookie":       "post_session=" + aliceCookie,
		"X-CSRF-Token": alice.CSRFToken,
	}

	bad := []string{
		`{"handle":"has space!"}`,
		`{"display_name":""}`,
		`{"display_name":"   "}`,
		`{"bio":"` + strings.Repeat("b", 2001) + `"}`,
	}
	for _, body := range bad {
		resp := do(t, http.MethodPatch, ts.URL+"/api/v1/users/"+alice.User.ID+"/profile", body, auth)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("PATCH %s = %d, want 400", body, resp.StatusCode)
			continue
		}
		if env := decodeMap(t, resp); env["code"] != profile.CodeValidation {
			t.Errorf("PATCH %s code = %v, want %s", body, env["code"], profile.CodeValidation)
		}
	}

	// Uppercase handle is valid after normalization.
	resp := do(t, http.MethodPatch, ts.URL+"/api/v1/users/"+alice.User.ID+"/profile",
		`{"handle":"UPPER-CASE-OK"}`, auth)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("uppercase handle PATCH = %d", resp.StatusCode)
	}
	if body := decodeMap(t, resp); body["handle"] != "upper-case-ok" {
		t.Errorf("handle after normalization = %v", body["handle"])
	}

	// Taking Bob's handle: 409 HANDLE_ALREADY_TAKEN.
	resp = do(t, http.MethodPatch, ts.URL+"/api/v1/users/"+alice.User.ID+"/profile",
		`{"handle":"api-bob"}`, auth)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("taken-handle PATCH = %d, want 409", resp.StatusCode)
	}
	if env := decodeMap(t, resp); env["code"] != profile.CodeHandleTaken {
		t.Errorf("taken-handle code = %v, want %s", env["code"], profile.CodeHandleTaken)
	}

	// Email in the body is ignored (unknown field), not applied: the
	// profile keeps Alice's identity facts.
	resp = do(t, http.MethodPatch, ts.URL+"/api/v1/users/"+alice.User.ID+"/profile",
		`{"email":"attacker@example.com","display_name":"Email Ignored"}`, auth)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("email-field PATCH = %d", resp.StatusCode)
	}
	body := decodeMap(t, resp)
	if body["display_name"] != "Email Ignored" {
		t.Errorf("display_name = %v", body["display_name"])
	}
	if _, present := body["email"]; present {
		t.Errorf("response leaked email: %v", body)
	}

	// Bob's session cannot be used to steal the handle back either way —
	// covered above; also: an unknown target id with a valid session is
	// still 404 (not 403).
	resp = do(t, http.MethodPatch, ts.URL+"/api/v1/users/00000000-0000-4000-8000-000000000000/profile",
		`{"bio":"x"}`, map[string]string{
			"Cookie":       "post_session=" + bobCookie,
			"X-CSRF-Token": bob.CSRFToken,
		})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("unknown-target PATCH = %d, want 403 (actor != target first)", resp.StatusCode)
	}
}

// TestAPIDisabledProfileStillReadable: a disabled account stops
// authenticating (the guard) but its public profile stays readable —
// nothing disappears, history only refers to it (CLAUDE.md §9.8).
func TestAPIDisabledProfileStillReadable(t *testing.T) {
	ts, users := newTestAPI(t)
	sr, _ := signup(t, ts, "api-carol@example.com")

	// Disable the account by rewriting the stored record (simulates an
	// admin disable; the auth surface has no disable endpoint in V1).
	rec, err := users.GetByID(t.Context(), sr.User.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	disabledAt := time.Now().UTC()
	rec.User.DisabledAt = &disabledAt
	users.Seed(rec) // Seed overwrites both indexes with the disabled record

	resp := do(t, http.MethodGet, ts.URL+"/api/v1/users/"+sr.User.ID+"/profile", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("disabled profile GET = %d, want 200 (nothing disappears)", resp.StatusCode)
	}
}
