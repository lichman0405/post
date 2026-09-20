package forkshttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/forks"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/memstore"
)

// The fork-route tests prove the transport through the REAL guard (signup
// issues the session, the write carries the session-bound CSRF token) and
// the real ServeMux pattern: what the handler hands the command, what it
// answers for each of the command's outcomes, and the one condition the
// contract states at this layer (a fork of a non-PUBLIC parent may not be
// created public). The authorization rules themselves are the service's
// (internal/application/forks/service_test.go); the end-to-end journey over
// real PostgreSQL and Gitea is tests/integration/external_fork_http_e2e_test.go.

const testProjectID = "11111111-2222-4333-8444-555555555555"

const testForkProjectID = "99999999-8888-4777-8666-555555555555"

// fakeForks is the command: what it was handed, and what it answers.
type fakeForks struct {
	out forks.ForkResult
	err error

	calls    int
	gotActor domain.User
	gotIn    forks.ForkRequest
}

func (f *fakeForks) Fork(_ context.Context, actor domain.User, in forks.ForkRequest) (forks.ForkResult, error) {
	f.calls++
	f.gotActor, f.gotIn = actor, in
	return f.out, f.err
}

// fakeProjects is the read gate the visibility condition runs. It records
// how often it was asked, which is how the tests show that the ordinary
// (private) request never consults it at all.
type fakeProjects struct {
	visibility domain.ProjectVisibility
	err        error

	calls int
	gotR  projects.Reader
}

func (f *fakeProjects) Get(_ context.Context, r projects.Reader, projectID string) (domain.Project, error) {
	f.calls++
	f.gotR = r
	if f.err != nil {
		return domain.Project{}, f.err
	}
	return domain.Project{ID: projectID, Visibility: f.visibility}, nil
}

// memstoreAuthAPI is the auth surface the fork tests put in front of the
// route: the production guard over in-memory stores, composed exactly as
// cmd/api composes it (the guard wraps the v1 subtree).
func memstoreAuthAPI(t *testing.T) *authhttp.API {
	t.Helper()
	return authhttp.New(authhttp.Deps{
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
}

// newForkServer composes the auth surface + the fork surface as cmd/api does
// (the guard wraps the v1 subtree), signs one user up and returns the server,
// the authed client, the actor id the auth surface minted and the session's
// CSRF token.
func newForkServer(t *testing.T, deps Deps) (ts *httptest.Server, client *http.Client, actorID, csrf string) {
	t.Helper()
	authAPI := memstoreAuthAPI(t)
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	New(deps).Register(mux)
	ts = httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)
	client, actorID, csrf = signupOn(t, ts)
	return ts, client, actorID, csrf
}

// signupOn signs one user up and returns the authed client, the actor id and
// the CSRF token the session is bound to.
func signupOn(t *testing.T, ts *httptest.Server) (*http.Client, string, string) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	authed := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := authed.Post(ts.URL+"/api/v1/auth/signup", "application/json",
		strings.NewReader(`{"email":"fork-http@example.com","password":"long-enough-password-1","handle":"fork-http","display_name":"Fork Http"}`))
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
	return authed, payload.User.ID, payload.CSRFToken
}

// forkURL is the route under test, spelled exactly as the contract spells it
// (specs/api/openapi.yaml: /projects/{projectId}/forks, servers: /api/v1).
func forkURL(server *httptest.Server, projectID string) string {
	return server.URL + "/api/v1/projects/" + projectID + "/forks"
}

// forkPost performs one fork write. body=="" sends no body at all (the
// contract's requestBody is not required); csrf=="" sends no CSRF token.
func forkPost(t *testing.T, client *http.Client, url, csrf, body string) *http.Response {
	t.Helper()
	var req *http.Request
	var err error
	if body == "" {
		req, err = http.NewRequest(http.MethodPost, url, nil)
	} else {
		req, err = http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	}
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// anonymousPost performs one fork write with no session at all.
func anonymousPost(t *testing.T, ts *httptest.Server, body string) *http.Response {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	var req *http.Request
	var err error
	if reader == nil {
		req, err = http.NewRequest(http.MethodPost, forkURL(ts, testProjectID), nil)
	} else {
		req, err = http.NewRequest(http.MethodPost, forkURL(ts, testProjectID), reader)
		req.Header.Set("Content-Type", "application/json")
	}
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodePayload[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// forkResult is the answer a successful command gives: the lineage row, the
// fork project and the fork branch.
func forkResult() forks.ForkResult {
	sha := "0f1e2d3c4b5a69788796a5b4c3d2e1f0"
	return forks.ForkResult{
		Fork: forks.Fork{
			ForkProjectID:   testForkProjectID,
			ParentProjectID: testProjectID,
			ForkedBy:        "actor-1",
			RelationType:    "forked_from",
			SourceBranchID:  "branch-parent-main",
			ForkBranchID:    "branch-fork",
			ForkedSHA:       &sha,
			CreatedAt:       time.Date(2026, 9, 19, 10, 30, 0, 0, time.UTC),
		},
		Project: domain.Project{
			ID: testForkProjectID, Slug: "mof-curie-0f1e2d3c", Name: "MOF Curie",
			Purpose: "fork of MOF", Visibility: domain.VisibilityPrivate,
		},
		Branch: domain.Branch{
			ID: "branch-fork", ProjectID: testForkProjectID, Name: "fork/main",
			GitRef: "refs/heads/fork/main", Visibility: domain.BranchVisibilityPrivate,
			Lifecycle: domain.BranchLifecycleActive,
		},
		Imported: true,
	}
}

// TestForkRouteFailsClosedWhenProvisioningIsDisabled: a deployment with no
// provider configuration must not write a project row and then panic on a nil
// adapter. The route answers 503 naming the missing keys, and the command is
// never reached — so in production the service never begins the transaction
// that would create the lineage-less project row.
func TestForkRouteFailsClosedWhenProvisioningIsDisabled(t *testing.T) {
	cmd := &fakeForks{out: forkResult()}
	ts, client, _, csrf := newForkServer(t, Deps{
		Forks:    cmd,
		Projects: &fakeProjects{visibility: domain.VisibilityPublic},
		Missing:  []string{"POST_GITEA_TOKEN", "POST_GITEA_WEBHOOK_URL"},
	})
	resp := forkPost(t, client, forkURL(ts, testProjectID), csrf, `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	env := decodePayload[errorEnvelope](t, resp)
	if env.Code != projects.CodeServiceUnavailable {
		t.Fatalf("code = %q, want %q", env.Code, projects.CodeServiceUnavailable)
	}
	for _, key := range []string{"POST_GITEA_TOKEN", "POST_GITEA_WEBHOOK_URL"} {
		if !strings.Contains(env.Message, key) {
			t.Fatalf("message = %q, want it to name the missing key %q", env.Message, key)
		}
	}
	if cmd.calls != 0 {
		t.Fatalf("the command was reached %d times: a disabled route must not start the transaction that writes the project row", cmd.calls)
	}
}

// TestForkRouteCreatesOnTheContractPath: the endpoint the contract defines
// answers 201 with the fork it created, the command received the
// authenticated actor and the project from the PATH (never from the body),
// and the body's optional fields travelled verbatim.
func TestForkRouteCreatesOnTheContractPath(t *testing.T) {
	cmd := &fakeForks{out: forkResult()}
	gate := &fakeProjects{visibility: domain.VisibilityPrivate}
	ts, client, actorID, csrf := newForkServer(t, Deps{Forks: cmd, Projects: gate})

	resp := forkPost(t, client, forkURL(ts, testProjectID), csrf,
		`{"name":"MOF Curie","purpose":"reproduce the 2024 isotherm","source_branch_id":"branch-parent-main","branch_name":"fork/main"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	got := decodePayload[forkPayload](t, resp)
	if got.Project.ID != testForkProjectID || got.Project.Slug != "mof-curie-0f1e2d3c" {
		t.Fatalf("project = %+v, want the fork project the command returned", got.Project)
	}
	if got.Fork.ForkProjectID != testForkProjectID || got.Fork.ParentProjectID != testProjectID ||
		got.Fork.RelationType != "forked_from" || got.Fork.ForkedSHA == nil {
		t.Fatalf("lineage = %+v, want the row the command returned", got.Fork)
	}
	if got.Fork.CreatedAt != "2026-09-19T10:30:00Z" {
		t.Fatalf("created_at = %q, want the RFC3339 rendering", got.Fork.CreatedAt)
	}
	if got.AlreadyForked || !got.Imported {
		t.Fatalf("already_forked/imported = %v/%v, want false/true", got.AlreadyForked, got.Imported)
	}
	if cmd.calls != 1 {
		t.Fatalf("command calls = %d, want 1", cmd.calls)
	}
	if cmd.gotActor.ID != actorID || cmd.gotActor.ID == "" {
		t.Fatalf("command actor = %q, want the session's user %q", cmd.gotActor.ID, actorID)
	}
	want := forks.ForkRequest{
		ProjectID: testProjectID, Name: "MOF Curie", Purpose: "reproduce the 2024 isotherm",
		SourceBranchID: "branch-parent-main", BranchName: "fork/main",
	}
	if cmd.gotIn != want {
		t.Fatalf("command request = %+v, want %+v", cmd.gotIn, want)
	}
	if gate.calls != 0 {
		t.Fatalf("the private request consulted the project reader %d times; the visibility condition is only about a PUBLIC fork", gate.calls)
	}
}

// TestForkRouteBodyIsOptional: the contract's requestBody is not required. An
// empty body is the ordinary "fork the parent's main, private" request, and
// every field reaches the command as the "not stated" zero value so the
// service applies its own defaults.
func TestForkRouteBodyIsOptional(t *testing.T) {
	cmd := &fakeForks{out: forkResult()}
	gate := &fakeProjects{visibility: domain.VisibilityPrivate}
	ts, client, _, csrf := newForkServer(t, Deps{Forks: cmd, Projects: gate})

	resp := forkPost(t, client, forkURL(ts, testProjectID), csrf, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if cmd.gotIn != (forks.ForkRequest{ProjectID: testProjectID}) {
		t.Fatalf("command request = %+v, want only the path's project", cmd.gotIn)
	}
}

// TestForkRouteIsIdempotentOnCanonicalStateNotOnAKeyLedger: a repeated
// request is answered 200 with the fork the command found, and the transport
// holds NO memory of the first answer — the second request reached the
// command again and reported the command's answer (a different fork project
// here, so a cached response could not produce it). The idempotency is the
// service's, against the recorded lineage row; this route carries no key
// ledger of its own.
func TestForkRouteIsIdempotentOnCanonicalStateNotOnAKeyLedger(t *testing.T) {
	first := forkResult()
	ts, client, _, csrf := newForkServer(t, Deps{
		Forks:    &fakeForks{out: first},
		Projects: &fakeProjects{visibility: domain.VisibilityPrivate},
	})

	resp := forkPost(t, client, forkURL(ts, testProjectID), csrf, `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", resp.StatusCode)
	}
	firstBody := decodePayload[forkPayload](t, resp)
	if firstBody.AlreadyForked {
		t.Fatalf("the first create reported already_forked")
	}

	// The same request again, against a command that now finds the pair's
	// fork: the second answer is 200 and carries what the command returned.
	repeat := forkResult()
	repeat.Fork.ForkProjectID = "77777777-6666-4555-8444-333333333333"
	repeat.Project.ID = repeat.Fork.ForkProjectID
	repeat.Branch.ProjectID = repeat.Fork.ForkProjectID
	repeat.AlreadyForked = true
	repeat.Imported = false
	cmd := &fakeForks{out: repeat}
	ts2, client2, _, csrf2 := newForkServer(t, Deps{Forks: cmd, Projects: &fakeProjects{visibility: domain.VisibilityPrivate}})
	resp2 := forkPost(t, client2, forkURL(ts2, testProjectID), csrf2, `{}`)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("repeat status = %d, want 200 (the contract's \"Already forked\")", resp2.StatusCode)
	}
	got := decodePayload[forkPayload](t, resp2)
	if !got.AlreadyForked || got.Imported {
		t.Fatalf("already_forked/imported = %v/%v, want true/false", got.AlreadyForked, got.Imported)
	}
	if got.Project.ID != repeat.Fork.ForkProjectID {
		t.Fatalf("project = %q, want the command's answer %q", got.Project.ID, repeat.Fork.ForkProjectID)
	}
	if cmd.calls != 1 {
		t.Fatalf("the repeat did not reach the command (%d calls): the route must not answer from a ledger of its own", cmd.calls)
	}
}

// TestForkRouteIsAnonymousRejected: docs/04 §2 — forks are for authenticated
// users, and the matrix's public_anonymous column denies create_branch. The
// guard answers before routing, so no request without a session ever reaches
// the command.
func TestForkRouteIsAnonymousRejected(t *testing.T) {
	cmd := &fakeForks{out: forkResult()}
	gate := &fakeProjects{visibility: domain.VisibilityPublic}
	ts, _, _, _ := newForkServer(t, Deps{Forks: cmd, Projects: gate})

	resp := anonymousPost(t, ts, `{"name":"MOF Curie"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	env := decodePayload[errorEnvelope](t, resp)
	if env.Code != codeUnauthenticated {
		t.Fatalf("code = %q, want %q", env.Code, codeUnauthenticated)
	}
	if cmd.calls != 0 {
		t.Fatalf("an anonymous request reached the command")
	}
}

// TestForkRouteOutcomes: every outcome the forks service reports maps onto
// the contract's status and the shared code, the command is never reached
// with a body this route could not read, and the message never leaks the Go
// error.
func TestForkRouteOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"forbidden", forks.ErrForbidden, http.StatusForbidden, codeForbidden},
		{"project not found", forks.ErrProjectNotFound, http.StatusNotFound, projects.CodeProjectNotFound},
		{"project not found (projects)", projects.ErrProjectNotFound, http.StatusNotFound, projects.CodeProjectNotFound},
		{"source branch not found", forks.ErrBranchNotFound, http.StatusNotFound, codeBranchNotFound},
		{"no source branch", forks.ErrNoSourceBranch, http.StatusNotFound, codeBranchNotFound},
		{"fork name taken", forks.ErrForkSlugTaken, http.StatusConflict, CodeForkNameTaken},
		{"validation", forks.ErrValidation, http.StatusBadRequest, projects.CodeValidationFailed},
		{"store outage", forks.ErrStore, http.StatusServiceUnavailable, projects.CodeServiceUnavailable},
		{"unknown", errors.New("boom"), http.StatusServiceUnavailable, projects.CodeServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &fakeForks{err: tc.err}
			ts, client, _, csrf := newForkServer(t, Deps{
				Forks:    cmd,
				Projects: &fakeProjects{visibility: domain.VisibilityPrivate},
			})
			resp := forkPost(t, client, forkURL(ts, testProjectID), csrf, `{}`)
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			env := decodePayload[errorEnvelope](t, resp)
			if env.Code != tc.code {
				t.Fatalf("code = %q, want %q", env.Code, tc.code)
			}
			for _, leak := range []string{"boom", "forks:", "persistence:"} {
				if strings.Contains(env.Message, leak) {
					t.Fatalf("message leaks the Go error: %q", env.Message)
				}
			}
		})
	}
}

// TestForkRouteFailsClosedWithoutTheCommand: the wiring is required. A
// deployment that forgot the fork command answers 503 rather than creating a
// fork no layer authorized.
func TestForkRouteFailsClosedWithoutTheCommand(t *testing.T) {
	ts, client, _, csrf := newForkServer(t, Deps{Projects: &fakeProjects{visibility: domain.VisibilityPublic}})
	resp := forkPost(t, client, forkURL(ts, testProjectID), csrf, `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

// TestForkRouteRefusesAPublicForkOfANonPublicParent: the one condition the
// contract states at this layer. The copy carries the parent's content, and a
// contributor of a private project holds allow on create_branch but deny on
// publish_private_to_public, so the route refuses rather than handing that
// class through a second door. The command is never reached — the refusal is
// the route's, and it happens before any fork exists.
func TestForkRouteRefusesAPublicForkOfANonPublicParent(t *testing.T) {
	cmd := &fakeForks{out: forkResult()}
	gate := &fakeProjects{visibility: domain.VisibilityPrivate}
	ts, client, _, csrf := newForkServer(t, Deps{Forks: cmd, Projects: gate})

	resp := forkPost(t, client, forkURL(ts, testProjectID), csrf, `{"visibility":"public"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	env := decodePayload[errorEnvelope](t, resp)
	if env.Code != codeForbidden {
		t.Fatalf("code = %q, want %q", env.Code, codeForbidden)
	}
	if cmd.calls != 0 {
		t.Fatalf("a refused public fork reached the command")
	}
	if gate.calls != 1 {
		t.Fatalf("project reader calls = %d, want 1: the condition is what decides", gate.calls)
	}
	if !gate.gotR.Authenticated || gate.gotR.UserID == "" {
		t.Fatalf("the reader ran as %+v, want the authenticated caller", gate.gotR)
	}
}

// TestForkRouteAllowsAPublicForkOfAPublicParent: the same condition, granted.
// A public parent's fork may be created public, and it is the COMMAND that
// then applies its own rules — the route passes the requested visibility
// through rather than deciding it.
func TestForkRouteAllowsAPublicForkOfAPublicParent(t *testing.T) {
	cmd := &fakeForks{out: forkResult()}
	gate := &fakeProjects{visibility: domain.VisibilityPublic}
	ts, client, _, csrf := newForkServer(t, Deps{Forks: cmd, Projects: gate})

	resp := forkPost(t, client, forkURL(ts, testProjectID), csrf, `{"visibility":"public"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if cmd.gotIn.Visibility != domain.VisibilityPublic {
		t.Fatalf("command visibility = %q, want public", cmd.gotIn.Visibility)
	}
}

// TestForkRoutePublicRequestOnAnUnreadableParentIsNotFound: the existence
// hiding rule survives the route-level condition. A caller who may not read
// the project is answered 404 — the same answer the command would give —
// never the 403 that would confirm the project exists.
func TestForkRoutePublicRequestOnAnUnreadableParentIsNotFound(t *testing.T) {
	cmd := &fakeForks{out: forkResult()}
	ts, client, _, csrf := newForkServer(t, Deps{
		Forks:    cmd,
		Projects: &fakeProjects{err: projects.ErrProjectNotFound},
	})
	resp := forkPost(t, client, forkURL(ts, testProjectID), csrf, `{"visibility":"public"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	env := decodePayload[errorEnvelope](t, resp)
	if env.Code != projects.CodeProjectNotFound {
		t.Fatalf("code = %q, want %q", env.Code, projects.CodeProjectNotFound)
	}
	if cmd.calls != 0 {
		t.Fatalf("the command ran for a project the caller cannot read")
	}
}

// TestForkRoutePublicRequestWithoutAReaderFailsClosed: an unevaluable
// condition is refused, never granted. A build that wired the command but not
// the project read cannot create a public fork of anything.
func TestForkRoutePublicRequestWithoutAReaderFailsClosed(t *testing.T) {
	cmd := &fakeForks{out: forkResult()}
	ts, client, _, csrf := newForkServer(t, Deps{Forks: cmd})
	resp := forkPost(t, client, forkURL(ts, testProjectID), csrf, `{"visibility":"public"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if cmd.calls != 0 {
		t.Fatalf("the fork was created with the visibility condition unevaluated")
	}
}

// TestForkRouteUnreadableBodyIsRefusedInPlace: a body that cannot be read is
// refused before the command runs. Accepting it would fork a line or a name
// the caller did not ask for.
func TestForkRouteUnreadableBodyIsRefusedInPlace(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"not json", `{"name":`},
		{"not an object", `"fork please"`},
		{"two objects", `{} {}`},
		{"visibility outside the enum", `{"visibility":"unlisted"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &fakeForks{out: forkResult()}
			gate := &fakeProjects{visibility: domain.VisibilityPublic}
			ts, client, _, csrf := newForkServer(t, Deps{Forks: cmd, Projects: gate})
			resp := forkPost(t, client, forkURL(ts, testProjectID), csrf, tc.body)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			env := decodePayload[errorEnvelope](t, resp)
			if env.Code != projects.CodeValidationFailed {
				t.Fatalf("code = %q, want %q", env.Code, projects.CodeValidationFailed)
			}
			if cmd.calls != 0 {
				t.Fatalf("the command ran on a body this route could not read")
			}
		})
	}
}

// TestForkRouteWithoutACsrfTokenIsRejected: the route is a write, so the
// session-bound CSRF token is required. Without it nothing is created.
func TestForkRouteWithoutACsrfTokenIsRejected(t *testing.T) {
	cmd := &fakeForks{out: forkResult()}
	ts, client, _, _ := newForkServer(t, Deps{
		Forks:    cmd,
		Projects: &fakeProjects{visibility: domain.VisibilityPublic},
	})
	resp := forkPost(t, client, forkURL(ts, testProjectID), "", `{"name":"MOF Curie"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if cmd.calls != 0 {
		t.Fatalf("a CSRF-less write reached the command")
	}
}

// TestForkRouteIsNotReachableByGET: the route is a write and only a write. A
// state-changing action reachable by GET would be reachable without the
// session-bound CSRF token, so the verb is part of the protection.
func TestForkRouteIsNotReachableByGET(t *testing.T) {
	cmd := &fakeForks{out: forkResult()}
	ts, client, _, _ := newForkServer(t, Deps{
		Forks:    cmd,
		Projects: &fakeProjects{visibility: domain.VisibilityPublic},
	})
	resp, err := client.Get(forkURL(ts, testProjectID))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", resp.StatusCode)
	}
	if cmd.calls != 0 {
		t.Fatalf("a GET reached the fork command")
	}
}

// TestForkRouteSendsTheCallersFieldsThroughThePathNotTheBody: the project is
// the {projectId} segment. A body that names a project of its own cannot
// redirect the fork: the field is not part of the wire shape at all, so the
// command receives the path's value.
func TestForkRouteSendsTheCallersFieldsThroughThePathNotTheBody(t *testing.T) {
	cmd := &fakeForks{out: forkResult()}
	ts, client, _, csrf := newForkServer(t, Deps{
		Forks:    cmd,
		Projects: &fakeProjects{visibility: domain.VisibilityPublic},
	})
	other := "22222222-3333-4444-8555-666666666666"
	resp := forkPost(t, client, forkURL(ts, testProjectID), csrf,
		`{"project_id":"`+other+`","actor_id":"someone-else","parent_project_id":"`+other+`"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if cmd.gotIn.ProjectID != testProjectID {
		t.Fatalf("command project = %q, want the path's %q", cmd.gotIn.ProjectID, testProjectID)
	}
}

// TestForkRouteURLIsTheContractsPath pins the path suffix this surface
// registers. The contract's path is /projects/{projectId}/forks under the
// /api/v1 server; a route mounted at another segment would answer 404 to the
// client the contract was written for.
func TestForkRouteURLIsTheContractsPath(t *testing.T) {
	u, err := url.Parse(forkURL(&httptest.Server{URL: "http://api.test"}, "pid"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/api/v1/projects/pid/forks" {
		t.Fatalf("path = %q", u.Path)
	}
}

// errorEnvelope is the standard failure document (docs/45, authhttp).
type errorEnvelope struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Retryable bool   `json:"retryable"`
}
