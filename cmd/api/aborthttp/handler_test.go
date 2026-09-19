package aborthttp

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
	"github.com/lichman0405/post/internal/application/aborts"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/memstore"
)

// The handler tests prove the transport through the REAL guard (signup issues
// the session, writes additionally carry the session-bound CSRF token) and the
// route's own path handling: the `:abort-proposal` suffix split off a
// rest-of-path wildcard, the required Idempotency-Key, the body decode, and
// the mapping of the abort package's own wire codes onto statuses. The fake
// command records what the handler passed it and answers canned outcomes —
// the abort rules themselves are the service's
// (internal/application/aborts/service_test.go) and the whole journey over
// real PostgreSQL is tests/integration/abort_e2e_test.go.

const (
	abortTestProjectID = "11111111-2222-4333-8444-555555555555"
	abortTestObjectID  = "99999999-8888-4777-8666-555555555555"
	abortTestVersionID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	abortTestKey       = "abort-proposal-key-0001"
)

// fakeCommand records the call and returns the configured outcome.
type fakeCommand struct {
	actor aborts.Actor
	in    aborts.Input
	calls int
	out   aborts.Result
	err   error
}

func (f *fakeCommand) AbortProposal(_ context.Context, actor aborts.Actor, in aborts.Input) (aborts.Result, error) {
	f.calls++
	f.actor, f.in = actor, in
	if f.err != nil {
		return aborts.Result{}, f.err
	}
	return f.out, nil
}

// newAbortTestServer composes the auth surface + the abort route exactly as
// cmd/api does (the guard wraps the v1 subtree), signs a user up through the
// real endpoint and returns the server, the authed client, the actor id and
// the CSRF token.
func newAbortTestServer(t *testing.T, cmd abortCommand) (*httptest.Server, *http.Client, string, string) {
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
		strings.NewReader(`{"email":"abort-handler@example.com","password":"long-enough-password-1","handle":"abort-handler","display_name":"Abort Handler"}`))
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

// abortPost performs one abort write with the CSRF token and the given
// Idempotency-Key ("" sends no key at all).
func abortPost(t *testing.T, client *http.Client, url, csrf, key, body string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodPost, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-CSRF-Token", csrf)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// envelope reads the JSON body and its error code ("" for a success).
func envelope(t *testing.T, resp *http.Response) (status int, body []byte, code string) {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var e struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, raw)
	}
	return resp.StatusCode, raw, e.Code
}

// raw reads the status and body without requiring the body to be JSON: a 404
// from the mux is plain text (http.NotFound), and a test that insists on an
// envelope there would be testing the helper rather than the route.
func raw(t *testing.T, resp *http.Response) (int, string) {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

// abortURL is the contract's path with the given last segment.
func abortURL(server *httptest.Server, segment string) string {
	return server.URL + "/api/v1/projects/" + abortTestProjectID + "/objects/" + segment
}

// abortVerbURL is abortURL with a well-formed last segment.
func abortVerbURL(server *httptest.Server) string {
	return abortURL(server, abortTestObjectID+":abort-proposal")
}

func abortTestBody() string {
	return fmt.Sprintf(`{"object_version_ref":"object_version:%s","reason_code":"superseded","explanation":"the re-run contradicts this claim"}`, abortTestVersionID)
}

// TestAbortRouteProposesOnTheContractPath: the endpoint the contract defines
// answers 201 with the proposal it created, and the command received the actor
// the guard resolved, the object id with the suffix stripped, the version ref
// with its platform prefix stripped, the body's four fields and the key.
func TestAbortRouteProposesOnTheContractPath(t *testing.T) {
	cmd := &fakeCommand{out: aborts.Result{
		ProjectID: abortTestProjectID, ObjectID: abortTestObjectID,
		AbortedVersionID: abortTestVersionID, AbortedVersionNo: 1,
		VersionID: "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff", VersionNo: 2,
		LifecycleState: string(domain.LifecycleAborted),
		BranchID:       "cccccccc-dddd-4eee-8fff-000000000000", BranchName: "abort/" + abortTestObjectID + "-deadbeef",
		PullRequestNumber: 7, PRState: string(domain.PullRequestStateOpen),
		ReasonCode: "superseded", Explanation: "the re-run contradicts this claim",
		ReplacementRef: "object_version:" + abortTestVersionID,
		DecidedBy:      "someone",
		DecidedAt:      time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}}
	ts, client, actorID, csrf := newAbortTestServer(t, cmd)

	resp := abortPost(t, client, abortVerbURL(ts), csrf, abortTestKey, abortTestBody())
	status, body, code := envelope(t, resp)
	if status != http.StatusCreated || code != "" {
		t.Fatalf("abort = %d %s (code %q), want 201", status, body, code)
	}
	var payload abortPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode the abort payload: %v (%s)", err, body)
	}
	if payload.ObjectID != abortTestObjectID || payload.VersionNo != 2 ||
		payload.LifecycleState != string(domain.LifecycleAborted) ||
		payload.PullRequestNumber != 7 || payload.Replayed {
		t.Fatalf("the payload does not carry the proposal: %+v", payload)
	}
	if payload.DecidedAt != "2026-09-19T12:00:00Z" {
		t.Fatalf("decided_at = %q, want the RFC3339 rendering of the decision time", payload.DecidedAt)
	}

	if cmd.calls != 1 {
		t.Fatalf("the command was called %d times, want 1", cmd.calls)
	}
	if cmd.actor.User.ID != actorID {
		t.Fatalf("the command saw actor %q, want the signed-up user %q", cmd.actor.User.ID, actorID)
	}
	if cmd.actor.IsAgent {
		t.Fatal("the transport told the command the request arrived as an agent; V1 authenticates sessions, so no HTTP request is one (the command's backstop is consulted regardless)")
	}
	if cmd.in.ProjectID != abortTestProjectID || cmd.in.ObjectID != abortTestObjectID {
		t.Fatalf("the command saw project %q object %q", cmd.in.ProjectID, cmd.in.ObjectID)
	}
	if cmd.in.ObjectVersionRef != abortTestVersionID {
		t.Fatalf("object_version_ref = %q, want the bare uuid %q (the `object_version:` prefix is the wire spelling)",
			cmd.in.ObjectVersionRef, abortTestVersionID)
	}
	if cmd.in.ReasonCode != "superseded" || cmd.in.Explanation != "the re-run contradicts this claim" {
		t.Fatalf("the command's record is %q/%q", cmd.in.ReasonCode, cmd.in.Explanation)
	}
	if cmd.in.IdempotencyKey != abortTestKey {
		t.Fatalf("the command saw key %q, want %q", cmd.in.IdempotencyKey, abortTestKey)
	}
}

// TestAbortRouteWithoutAKey: the Idempotency-Key is required here even though
// the route's own contract declaration does not name it (the components
// parameter is required and minLength 8; see the package doc). A request
// without one is refused rather than performed without the ability to repeat
// safely, and the command is never reached.
func TestAbortRouteWithoutAKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"missing", ""},
		{"too short", "short"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &fakeCommand{}
			ts, client, _, csrf := newAbortTestServer(t, cmd)
			status, body, code := envelope(t, abortPost(t, client, abortVerbURL(ts), csrf, tc.key, abortTestBody()))
			if status != http.StatusBadRequest || code != aborts.CodeValidationFailed {
				t.Fatalf("a %s Idempotency-Key answered %d %s (code %q), want 400 %s",
					tc.name, status, body, code, aborts.CodeValidationFailed)
			}
			if cmd.calls != 0 {
				t.Fatalf("the command was reached %d times; a request without a usable key is refused at the transport", cmd.calls)
			}
		})
	}
}

// TestAbortRoutePathHandling: the route's last segment is a rest-of-path
// wildcard, because Go's ServeMux cannot express the partial wildcard the
// contract's `{objectId}:abort-proposal` would be. Everything that segment
// could carry that is not a valid abort URL answers 404 — the URL names
// nothing — and the command is never reached.
func TestAbortRoutePathHandling(t *testing.T) {
	for _, tc := range []struct {
		name    string
		segment string
	}{
		{"no suffix", abortTestObjectID},
		{"empty id", ":abort-proposal"},
		{"a swallowed path", abortTestObjectID + "/versions:abort-proposal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &fakeCommand{}
			ts, client, _, csrf := newAbortTestServer(t, cmd)
			status, body := raw(t, abortPost(t, client, abortURL(ts, tc.segment), csrf, abortTestKey, abortTestBody()))
			if status != http.StatusNotFound {
				t.Fatalf("the segment %q answered %d %s, want 404", tc.segment, status, body)
			}
			if cmd.calls != 0 {
				t.Fatalf("the command was reached %d times for a URL that names nothing", cmd.calls)
			}
		})
	}
}

// TestAbortRouteRefusesUnauthenticatedRequests: the matrix's anonymous cell
// for abort_main_object is `deny`, and the guard is where that cell is
// enforced on every state-changing route — before the handler, so the command
// is never reached and no target is read.
func TestAbortRouteRefusesUnauthenticatedRequests(t *testing.T) {
	cmd := &fakeCommand{}
	ts, _, _, _ := newAbortTestServer(t, cmd)
	status, body, code := envelope(t, abortPost(t, &http.Client{}, abortVerbURL(ts), "", "", abortTestBody()))
	if status != http.StatusUnauthorized || code != "AUTH_UNAUTHENTICATED" {
		t.Fatalf("an anonymous abort answered %d %s (code %q), want 401 AUTH_UNAUTHENTICATED", status, body, code)
	}
	if cmd.calls != 0 {
		t.Fatalf("the command was reached %d times by an anonymous request", cmd.calls)
	}
}

// TestAbortRouteRejectsAMalformedBody.
func TestAbortRouteRejectsAMalformedBody(t *testing.T) {
	cmd := &fakeCommand{}
	ts, client, _, csrf := newAbortTestServer(t, cmd)
	status, body, code := envelope(t, abortPost(t, client, abortVerbURL(ts), csrf, abortTestKey, `{"object_version_ref":`))
	if status != http.StatusBadRequest || code != aborts.CodeValidationFailed {
		t.Fatalf("a malformed body answered %d %s (code %q), want 400 %s", status, body, code, aborts.CodeValidationFailed)
	}
	if cmd.calls != 0 {
		t.Fatalf("the command was reached %d times with an unparseable body", cmd.calls)
	}
}

// TestAbortRouteWireCodes maps each outcome the command can report onto the
// status the client sees. The authorization refusal is the one that matters
// most: every class the matrix denies — including a caller naming a project
// that does not exist — answers AUTH_FORBIDDEN, so the response cannot be
// read as "this project exists and you may not touch it".
func TestAbortRouteWireCodes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"validation", fmt.Errorf("%w: reason_code must be 1..64 characters of [a-z0-9_]", aborts.ErrValidation), http.StatusBadRequest, aborts.CodeValidationFailed},
		{"forbidden", aborts.ErrForbidden, http.StatusForbidden, aborts.CodeForbidden},
		{"agent", &aborts.AgentNotPermittedError{Action: "abort_main_object"}, http.StatusForbidden, aborts.CodeAgentDenied},
		{"unknown object", aborts.ErrObjectNotFound, http.StatusNotFound, aborts.CodeObjectNotFound},
		{"unknown version", aborts.ErrVersionNotFound, http.StatusNotFound, aborts.CodeVersionNotFound},
		{"not on main", aborts.ErrNotMainObject, http.StatusConflict, aborts.CodeNotMainObject},
		{"moved log", aborts.ErrConflict, http.StatusConflict, aborts.CodeConflict},
		{"store", errWrapped(aborts.ErrStore, errors.New("dial tcp: connection refused")), http.StatusServiceUnavailable, aborts.CodeServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &fakeCommand{err: tc.err}
			ts, client, _, csrf := newAbortTestServer(t, cmd)
			status, body, code := envelope(t, abortPost(t, client, abortVerbURL(ts), csrf, abortTestKey, abortTestBody()))
			if status != tc.status || code != tc.code {
				t.Fatalf("the %s outcome answered %d %s (code %q), want %d %s", tc.name, status, body, code, tc.status, tc.code)
			}
			if tc.code == aborts.CodeServiceUnavailable && strings.Contains(string(body), "connection refused") {
				t.Fatalf("the store's own message reached the wire: %s", body)
			}
		})
	}
}

// errWrapped keeps the cause the store outcome carries while answering as
// ErrStore, the shape the command's own mapping produces.
func errWrapped(sentinel, cause error) error {
	return fmt.Errorf("%w: %v", sentinel, cause)
}

// TestAbortRouteWithoutACommand: an unwired command is a 503, not a panic and
// not a silent 201.
func TestAbortRouteWithoutACommand(t *testing.T) {
	ts, client, _, csrf := newAbortTestServer(t, nil)
	status, body, code := envelope(t, abortPost(t, client, abortVerbURL(ts), csrf, abortTestKey, abortTestBody()))
	if status != http.StatusServiceUnavailable || code != aborts.CodeServiceUnavailable {
		t.Fatalf("an unwired abort route answered %d %s (code %q), want 503", status, body, code)
	}
}
