package freezehttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/mainfreeze"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/memstore"
)

// The handler tests prove the transport through the REAL guard (signup
// issues the session and the session-bound CSRF token), the REAL ServeMux
// pattern (the contract's literal "main:freeze" suffix on the {projectId}
// segment), the contract-required Idempotency-Key, and the mapping of the
// freeze package's own wire codes onto statuses. The fake command records
// what it was handed and answers canned outcomes — the freeze rules
// themselves are the command's (internal/application/mainfreeze) and the
// governance end-to-end journey over real PostgreSQL and a real Gitea is
// tests/integration/freeze_main_e2e_test.go.

const (
	freezeTestProjectID = "11111111-2222-4333-8444-555555555555"
	freezeTestKey       = "freeze-idempotency-key-0001"
)

// fakeCommand records the call and returns the configured outcome.
type fakeCommand struct {
	actor mainfreeze.Actor
	in    mainfreeze.Input
	calls int
	err   error
}

func (f *fakeCommand) Freeze(_ context.Context, actor mainfreeze.Actor, in mainfreeze.Input) (mainfreeze.Result, error) {
	f.calls++
	f.actor, f.in = actor, in
	if f.err != nil {
		return mainfreeze.Result{}, f.err
	}
	return mainfreeze.Result{ProjectID: in.ProjectID, MainFrozen: true}, nil
}

// uncoded is an outcome that names a wire code the transport has no status
// for: the handler must answer 500 rather than invent one.
type uncoded struct{}

func (uncoded) Error() string { return "mainfreeze: outcome with no status" }
func (uncoded) Code() string  { return "NOT_A_REAL_CODE" }

// newFreezeTestServer composes the auth surface + the freeze route exactly
// as cmd/api does (the guard wraps the v1 subtree), signs a user up and
// returns the server, the authed client, the actor id and the CSRF token.
func newFreezeTestServer(t *testing.T, cmd freezeCommand) (*httptest.Server, *http.Client, string, string) {
	t.Helper()
	authAPI := authhttp.New(authhttp.Deps{
		Users:    memstore.NewUsers(),
		Sessions: memstore.NewSessions(),
		Limiter:  memstore.NewLimiter(),
		Cfg: authn.Config{
			WebOrigin:          "http://web.test",
			SessionTTL:         time.Hour,
			LoginLimitPerEmail: 1000,
			LoginLimitPerIP:    10000,
			LoginWindow:        time.Minute,
			SignupLimitPerIP:   10000,
		},
	})
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	New(Deps{Command: cmd}).Register(mux)
	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	authed := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := authed.Post(ts.URL+"/api/v1/auth/signup", "application/json",
		strings.NewReader(`{"email":"freeze-handler@example.com","password":"long-enough-password-1","handle":"freeze-handler","display_name":"Freeze Handler"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("signup = %d", resp.StatusCode)
	}
	var payload struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return ts, authed, payload.User.ID, payload.CSRFToken
}

// freezePost performs one freeze write with the CSRF token and the given
// Idempotency-Key ("" sends no key at all). projectID and suffix build the
// path, so a test can also ask for a path that must not exist.
func freezePost(t *testing.T, client *http.Client, base, csrf, key, projectID, suffix string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/projects/"+projectID+suffix, nil)
	if err != nil {
		t.Fatal(err)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// envelope reads the JSON body, its error code ("" for a success) and the
// raw text, which is what the leak assertions need.
func envelope(t *testing.T, resp *http.Response) (status int, code, raw string) {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var e struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, body)
	}
	return resp.StatusCode, e.Code, string(body)
}

// TestFreezeRouteFreezesOnTheContractPath: the endpoint the contract defines
// (specs/api/openapi.yaml "POST /projects/{projectId}/main:freeze") answers
// 200 with the frozen state, and the command received the actor the guard
// resolved, the path's project id and the request's Idempotency-Key.
func TestFreezeRouteFreezesOnTheContractPath(t *testing.T) {
	cmd := &fakeCommand{}
	ts, client, actorID, csrf := newFreezeTestServer(t, cmd)

	resp := freezePost(t, client, ts.URL, csrf, freezeTestKey, freezeTestProjectID, "/main:freeze")
	status, code, raw := envelope(t, resp)
	if status != http.StatusOK || code != "" {
		t.Fatalf("freeze = %d (code %q) %s, want 200", status, code, raw)
	}
	var payload struct {
		ProjectID     string `json:"project_id"`
		MainFrozen    bool   `json:"main_frozen"`
		AlreadyFrozen bool   `json:"already_frozen"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ProjectID != freezeTestProjectID || !payload.MainFrozen || payload.AlreadyFrozen {
		t.Fatalf("payload = %s, want the project frozen by this call", raw)
	}
	if cmd.calls != 1 {
		t.Fatalf("command calls = %d, want 1", cmd.calls)
	}
	if cmd.in.ProjectID != freezeTestProjectID || cmd.in.IdempotencyKey != freezeTestKey {
		t.Fatalf("command input = %+v", cmd.in)
	}
	if cmd.actor.User.ID != actorID {
		t.Fatalf("command actor = %q, want the authenticated session's user %q", cmd.actor.User.ID, actorID)
	}
	if cmd.actor.IsAgent {
		t.Fatal("the transport told the command a human session was an agent")
	}
}

// TestFreezeRouteRequiresTheContractIdempotencyKey: the key is required and
// at least 8 characters (components.parameters.IdempotencyKey). A request
// without one is refused before the command runs — the freeze is not
// performed and then reported as unkeyed.
func TestFreezeRouteRequiresTheContractIdempotencyKey(t *testing.T) {
	cases := map[string]string{
		"missing": "",
		"short":   "1234567",
	}
	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			cmd := &fakeCommand{}
			ts, client, _, csrf := newFreezeTestServer(t, cmd)
			resp := freezePost(t, client, ts.URL, csrf, key, freezeTestProjectID, "/main:freeze")
			status, code, raw := envelope(t, resp)
			if status != http.StatusBadRequest || code != mainfreeze.CodeValidationFailed {
				t.Fatalf("freeze with a %s key = %d (code %q) %s, want 400 %s",
					name, status, code, raw, mainfreeze.CodeValidationFailed)
			}
			if cmd.calls != 0 {
				t.Fatalf("command reached %d times without a key", cmd.calls)
			}
		})
	}
	// The boundary itself: 8 characters is admissible (the contract's
	// minLength is a minimum, not an exclusive bound).
	cmd := &fakeCommand{}
	ts, client, _, csrf := newFreezeTestServer(t, cmd)
	resp := freezePost(t, client, ts.URL, csrf, strings.Repeat("k", mainfreeze.MinIdempotencyKeyLen), freezeTestProjectID, "/main:freeze")
	if status, code, raw := envelope(t, resp); status != http.StatusOK {
		t.Fatalf("a key at the contract's minimum was refused = %d (code %q) %s", status, code, raw)
	}
}

// TestFreezeRouteRequiresAuthentication: the route sits behind the session
// guard like every other product route, and a caller with no session is
// answered 401 without the command being reached.
func TestFreezeRouteRequiresAuthentication(t *testing.T) {
	cmd := &fakeCommand{}
	ts, _, _, _ := newFreezeTestServer(t, cmd)
	anonymous := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/projects/"+freezeTestProjectID+"/main:freeze", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Idempotency-Key", freezeTestKey)
	resp, err := anonymous.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	status, code, raw := envelope(t, resp)
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated freeze = %d (code %q) %s, want 401", status, code, raw)
	}
	if cmd.calls != 0 {
		t.Fatalf("command reached %d times for an unauthenticated caller", cmd.calls)
	}
}

// TestFreezeRouteRefusesStateChangesWithoutCSRF: the freeze is a state
// change, so the session-bound token is enforced by the guard, not by this
// package — asserted here because a governance action that a cross-site
// page could trigger would be worse than most.
func TestFreezeRouteRefusesStateChangesWithoutCSRF(t *testing.T) {
	cmd := &fakeCommand{}
	ts, client, _, _ := newFreezeTestServer(t, cmd)
	resp := freezePost(t, client, ts.URL, "", freezeTestKey, freezeTestProjectID, "/main:freeze")
	status, code, raw := envelope(t, resp)
	if status != http.StatusForbidden || code != authn.CodeCSRFFailed {
		t.Fatalf("freeze without CSRF = %d (code %q) %s, want 403 %s", status, code, raw, authn.CodeCSRFFailed)
	}
	if cmd.calls != 0 {
		t.Fatalf("command reached %d times without a CSRF token", cmd.calls)
	}
}

// TestFreezeRouteMapsEachRefusal: one outcome, one status, one stable code
// (docs/45). The authorization refusal and the agent backstop are asserted
// to answer DIFFERENT codes: a client that could not tell them apart could
// not tell which line of defence refused it, and neither could a test.
func TestFreezeRouteMapsEachRefusal(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{
			name:   "authorization refused",
			err:    mainfreeze.ErrForbidden,
			status: http.StatusForbidden,
			code:   mainfreeze.CodeForbidden,
		},
		{
			name:   "agent refused by the domain backstop",
			err:    &mainfreeze.AgentNotPermittedError{Action: "freeze_main"},
			status: http.StatusForbidden,
			code:   mainfreeze.CodeAgentFreezeDenied,
		},
		{
			name:   "the policy in force refuses",
			err:    &mainfreeze.PolicyRefusedError{Rule: domain.RuleMainProtected, Found: true, Reason: "policy sets main_protected to false"},
			status: http.StatusForbidden,
			code:   mainfreeze.CodePolicyRefused,
		},
		{
			name:   "store unavailable",
			err:    fmt.Errorf("%w: %v", mainfreeze.ErrStore, errors.New("pq: connection refused to 10.0.0.7:5432")),
			status: http.StatusServiceUnavailable,
			code:   mainfreeze.CodeServiceUnavailable,
		},
		{
			name:   "an error the transport does not know",
			err:    errors.New("pq: connection refused to 10.0.0.7:5432"),
			status: http.StatusInternalServerError,
			code:   "INTERNAL_ERROR",
		},
		{
			name:   "a coded outcome with no status",
			err:    uncoded{},
			status: http.StatusInternalServerError,
			code:   "INTERNAL_ERROR",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &fakeCommand{err: tc.err}
			ts, client, _, csrf := newFreezeTestServer(t, cmd)
			resp := freezePost(t, client, ts.URL, csrf, freezeTestKey, freezeTestProjectID, "/main:freeze")
			status, code, raw := envelope(t, resp)
			if status != tc.status || code != tc.code {
				t.Fatalf("freeze = %d (code %q) %s, want %d %s", status, code, raw, tc.status, tc.code)
			}
			// The driver's own text must never reach the client.
			if strings.Contains(raw, "10.0.0.7") {
				t.Fatalf("the store's own text reached the wire: %s", raw)
			}
		})
	}
}

// TestFreezeRouteAnswersUnknownProjectAsPermissionRefusal is acceptance
// criterion 7 at the transport: an unknown project id must NOT be answered
// 404. The route runs no project read gate, so the command's authorization
// answers it, and — because the command hides existence — the transport has
// no other outcome available to it.
func TestFreezeRouteAnswersUnknownProjectAsPermissionRefusal(t *testing.T) {
	cmd := &fakeCommand{err: mainfreeze.ErrForbidden}
	ts, client, _, csrf := newFreezeTestServer(t, cmd)
	unknown := "99999999-9999-4999-8999-999999999999"
	resp := freezePost(t, client, ts.URL, csrf, freezeTestKey, unknown, "/main:freeze")
	status, code, raw := envelope(t, resp)
	if status == http.StatusNotFound {
		t.Fatalf("an unknown project id was answered 404, disclosing its absence: %s", raw)
	}
	if status != http.StatusForbidden || code != mainfreeze.CodeForbidden {
		t.Fatalf("unknown project = %d (code %q) %s, want 403 %s", status, code, raw, mainfreeze.CodeForbidden)
	}
}

// TestFreezeRouteHasNoUnfreezePath: the contract has exactly one direction,
// and the surface has exactly one route. Anything else — a second verb on
// the same path, a plausible-looking ":unfreeze" — is not registered at
// all, which is how "no back door" is observable from outside.
func TestFreezeRouteHasNoUnfreezePath(t *testing.T) {
	cmd := &fakeCommand{}
	ts, client, _, csrf := newFreezeTestServer(t, cmd)
	cases := map[string]struct{ method, suffix string }{
		"POST :unfreeze": {http.MethodPost, "/main:unfreeze"},
		"POST main":      {http.MethodPost, "/main"},
		"DELETE :freeze": {http.MethodDelete, "/main:freeze"},
		"GET :freeze":    {http.MethodGet, "/main:freeze"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, ts.URL+"/api/v1/projects/"+freezeTestProjectID+tc.suffix, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Idempotency-Key", freezeTestKey)
			req.Header.Set("X-CSRF-Token", csrf)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("%s %s = %d %s, want 404 (no such route)", tc.method, tc.suffix, resp.StatusCode, body)
			}
		})
	}
	if cmd.calls != 0 {
		t.Fatalf("the command was reached %d times by a path that must not exist", cmd.calls)
	}
}
