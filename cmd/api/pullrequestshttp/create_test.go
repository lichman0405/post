package pullrequestshttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/forks"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/memstore"
)

// The open-pull-request tests prove the transport through the REAL guard
// (signup issues the session, the write carries the session-bound CSRF token)
// and the real ServeMux patterns: the collection's POST route, the
// contract-required Idempotency-Key, the mapping of the forks package's own
// outcomes onto statuses, and that the caller's identity — never a body field
// — is what the command authorizes. The fake command records what the handler
// passed it; the authorization rules themselves are the forks service's
// (internal/application/forks/service_test.go) and the end-to-end journey is
// tests/integration/pr_flow_e2e_test.go.
const createTestProjectID = "11111111-2222-4333-8444-555555555555"

// createKey is a well-formed Idempotency-Key (>= MinCreationKeyLen).
const createKey = "pr-create-00000001"

// fakeCreator is the PRCreator: it records the actor and the request the
// handler passed and answers a canned outcome.
type fakeCreator struct {
	out      domain.PullRequest
	err      error
	calls    int
	gotActor domain.User
	gotIn    forks.OpenPRRequest
}

func (f *fakeCreator) OpenExternalPR(_ context.Context, actor domain.User, in forks.OpenPRRequest) (domain.PullRequest, error) {
	f.calls++
	f.gotActor, f.gotIn = actor, in
	return f.out, f.err
}

// memstoreAuthAPI is the auth surface the create tests put in front of the
// route: the production guard, with in-memory stores. It is the same
// composition cmd/api performs (guard wraps the v1 subtree), so CSRF, the
// session cookie and the principal the handler reads are the production ones.
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

// signupOn signs one user up on the guarded server and returns the authed
// client, the actor id the auth surface minted and the session's CSRF token.
func signupOn(t *testing.T, ts *httptest.Server) (*http.Client, string, string) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	authed := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := authed.Post(ts.URL+"/api/v1/auth/signup", "application/json",
		strings.NewReader(`{"email":"pr-create@example.com","password":"long-enough-password-1","handle":"pr-create","display_name":"PR Create"}`))
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

// newCreateTestServer composes the auth surface + the pull-request surface
// exactly as cmd/api does (the guard wraps the v1 subtree), signs a user up
// and returns the server, the authed client, the actor id and the CSRF token.
func newCreateTestServer(t *testing.T, create PRCreator) (ts *httptest.Server, client *http.Client, actorID, csrf string) {
	t.Helper()
	return newCreateTestServerWithPRs(t, &fakePRs{}, create)
}

// newCreateTestServerWithPRs is newCreateTestServer with the read surface
// supplied, for the tests that assert the write route did not disturb it.
func newCreateTestServerWithPRs(t *testing.T, prs PullRequests, create PRCreator) (ts *httptest.Server, client *http.Client, actorID, csrf string) {
	t.Helper()
	authAPI := memstoreAuthAPI(t)
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	New(Deps{PullRequests: prs, Create: create, Projects: &fakeProjectGate{}}).Register(mux)
	ts = httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)
	client, actorID, csrf = signupOn(t, ts)
	return ts, client, actorID, csrf
}

// createPost performs one open-pull-request write. body=="" sends no body;
// key=="" sends no Idempotency-Key; csrf=="" sends no CSRF token.
func createPost(t *testing.T, client *http.Client, url, csrf, key, body string) *http.Response {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	var req *http.Request
	var err error
	if reader == nil {
		req, err = http.NewRequest(http.MethodPost, url, nil)
	} else {
		req, err = http.NewRequest(http.MethodPost, url, reader)
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
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func createURL(server *httptest.Server, projectID string) string {
	return server.URL + "/api/v1/projects/" + projectID + "/pull-requests"
}

// createBody is the wire body of one valid request.
const createBody = `{"source_branch_id":"branch-src","target_branch_id":"branch-main","title":"annealing protocol","body":"the evidence is on the branch"}`

// TestOpenPullRequestRouteCreatesOnTheContractPath: the endpoint the contract
// defines answers 201 with the proposal it opened, and the command received
// the authenticated actor, the project from the PATH (never from the body) and
// the creation key from the header.
func TestOpenPullRequestRouteCreatesOnTheContractPath(t *testing.T) {
	creator := &fakeCreator{out: domain.PullRequest{
		ID: "pr-1", ProjectID: createTestProjectID, Number: 12,
		SourceBranchID: "branch-src", TargetBranchID: "branch-main",
		BaseStateID: "state-main", ProposedStateID: "state-src",
		Title: "annealing protocol", Body: "the evidence is on the branch",
		State: domain.PullRequestStateOpen, CreatedBy: "someone",
	}}
	ts, client, actorID, csrf := newCreateTestServer(t, creator)

	// The path names a project the body does not: a caller naming a project in
	// the body must not be able to steer the command (the body is decoded into
	// a shape with no project field at all, so this is a property of the type).
	body := `{"project_id":"other","source_branch_id":"branch-src","target_branch_id":"branch-main","title":"annealing protocol","body":"the evidence is on the branch"}`
	resp := createPost(t, client, createURL(ts, createTestProjectID), csrf, createKey, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var got prPayload
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Number != 12 || got.State != string(domain.PullRequestStateOpen) {
		t.Fatalf("payload = %+v, want the opened proposal", got)
	}
	if creator.calls != 1 {
		t.Fatalf("command called %d times, want 1", creator.calls)
	}
	if creator.gotActor.ID != actorID || creator.gotActor.ID == "" {
		t.Fatalf("command actor = %q, want the guard's %q", creator.gotActor.ID, actorID)
	}
	in := creator.gotIn
	if in.ProjectID != createTestProjectID {
		t.Fatalf("command project = %q, want the path's %q", in.ProjectID, createTestProjectID)
	}
	if in.SourceBranchID != "branch-src" || in.TargetBranchID != "branch-main" {
		t.Fatalf("command branches = %q → %q", in.SourceBranchID, in.TargetBranchID)
	}
	if in.Title != "annealing protocol" || in.Body != "the evidence is on the branch" {
		t.Fatalf("command text = %q / %q", in.Title, in.Body)
	}
	if in.CreationKey != createKey {
		t.Fatalf("command creation key = %q, want %q", in.CreationKey, createKey)
	}
}

// TestOpenPullRequestRequiresASession: the guard refuses an anonymous write
// before routing — a caller without a session never reaches the command.
func TestOpenPullRequestRequiresASession(t *testing.T) {
	creator := &fakeCreator{}
	ts, _, _, _ := newCreateTestServer(t, creator)

	resp := createPost(t, http.DefaultClient, createURL(ts, createTestProjectID), "", createKey, createBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if creator.calls != 0 {
		t.Fatalf("command reached %d times without a session", creator.calls)
	}
}

// TestOpenPullRequestRequiresTheCreationKey: the route's Idempotency-Key is
// required by the contract (components.parameters.IdempotencyKey) and the
// handler enforces it in place — a request that cannot carry a key cannot be
// given the promise that a repeat returns the first proposal.
func TestOpenPullRequestRequiresTheCreationKey(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		creator := &fakeCreator{}
		ts, client, _, csrf := newCreateTestServer(t, creator)
		resp := createPost(t, client, createURL(ts, createTestProjectID), csrf, "", createBody)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		got := decode[errorEnvelope](t, resp)
		if got.Code != pullrequests.CodeValidation {
			t.Fatalf("code = %q, want %q", got.Code, pullrequests.CodeValidation)
		}
		if creator.calls != 0 {
			t.Fatalf("command reached without a key")
		}
	})
	t.Run("too short", func(t *testing.T) {
		creator := &fakeCreator{}
		ts, client, _, csrf := newCreateTestServer(t, creator)
		// One character below the contract's minLength.
		short := strings.Repeat("k", pullrequests.MinCreationKeyLen-1)
		resp := createPost(t, client, createURL(ts, createTestProjectID), csrf, short, createBody)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		if creator.calls != 0 {
			t.Fatalf("command reached with a key the contract refuses")
		}
	})
	t.Run("at the minimum the request goes through", func(t *testing.T) {
		creator := &fakeCreator{out: domain.PullRequest{Number: 3}}
		ts, client, _, csrf := newCreateTestServer(t, creator)
		key := strings.Repeat("k", pullrequests.MinCreationKeyLen)
		resp := createPost(t, client, createURL(ts, createTestProjectID), csrf, key, createBody)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d, want 201", resp.StatusCode)
		}
		if creator.gotIn.CreationKey != key {
			t.Fatalf("creation key = %q, want %q", creator.gotIn.CreationKey, key)
		}
	})
}

// TestOpenPullRequestRejectsAMalformedBody: a body that is not the documented
// JSON object is a 400 in the shared validation code, and reaches no command.
func TestOpenPullRequestRejectsAMalformedBody(t *testing.T) {
	creator := &fakeCreator{}
	ts, client, _, csrf := newCreateTestServer(t, creator)

	resp := createPost(t, client, createURL(ts, createTestProjectID), csrf, createKey, `{"source_branch_id":`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	got := decode[errorEnvelope](t, resp)
	if got.Code != pullrequests.CodeValidation {
		t.Fatalf("code = %q, want %q", got.Code, pullrequests.CodeValidation)
	}
	if creator.calls != 0 {
		t.Fatalf("command reached with an undecodable body")
	}
}

// TestOpenPullRequestOutcomes: every outcome the forks/service layer reports
// maps onto the documented status and the shared code, and the message never
// leaks the Go error.
func TestOpenPullRequestOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"matrix refusal", forks.ErrForbidden, http.StatusForbidden, codeForbidden},
		{"project not found", forks.ErrProjectNotFound, http.StatusNotFound, projects.CodeProjectNotFound},
		{"project not found (projects)", projects.ErrProjectNotFound, http.StatusNotFound, projects.CodeProjectNotFound},
		{"branch not found", pullrequests.ErrBranchNotFound, http.StatusNotFound, pullrequests.CodeBranchNotFound},
		{"branch not active", &pullrequests.BranchNotActiveError{BranchID: "branch-src", Lifecycle: "merged"}, http.StatusConflict, pullrequests.CodeBranchNotActive},
		{"branch head missing", pullrequests.ErrBranchHeadMissing, http.StatusConflict, pullrequests.CodeBranchHeadMissing},
		{"unstructured branch changes", pullrequests.ErrBranchUnstructuredChanges, http.StatusConflict, pullrequests.CodeBranchUnstructuredChanges},
		{"validation", pullrequests.ErrValidation, http.StatusBadRequest, pullrequests.CodeValidation},
		{"forks validation", forks.ErrValidation, http.StatusBadRequest, pullrequests.CodeValidation},
		{"store outage", pullrequests.ErrStore, http.StatusServiceUnavailable, pullrequests.CodeUnavailable},
		{"unknown", errors.New("boom"), http.StatusServiceUnavailable, pullrequests.CodeUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			creator := &fakeCreator{err: tc.err}
			ts, client, _, csrf := newCreateTestServer(t, creator)
			resp := createPost(t, client, createURL(ts, createTestProjectID), csrf, createKey, createBody)
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			got := decode[errorEnvelope](t, resp)
			if got.Code != tc.code {
				t.Fatalf("code = %q, want %q", got.Code, tc.code)
			}
			if strings.Contains(got.Message, "boom") || strings.Contains(got.Message, "forks") ||
				strings.Contains(got.Message, "pullrequests") {
				t.Fatalf("message leaks the Go error: %q", got.Message)
			}
		})
	}
}

// TestOpenPullRequestWithoutCommandFailsClosed: the wiring is required. A
// deployment that forgot it answers 503 rather than opening proposals with no
// authorization at all.
func TestOpenPullRequestWithoutCommandFailsClosed(t *testing.T) {
	authAPI := memstoreAuthAPI(t)
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	New(Deps{PullRequests: &fakePRs{}, Projects: &fakeProjectGate{}}).Register(mux)
	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)

	client, _, csrf := signupOn(t, ts)
	resp := createPost(t, client, createURL(ts, createTestProjectID), csrf, createKey, createBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

// TestOpenPullRequestKeepsTheReadRoutesUnchanged: the write route did not eat
// the collection's GET (the same path, the other verb) nor the {number} detail
// route.
func TestOpenPullRequestKeepsTheReadRoutesUnchanged(t *testing.T) {
	prs := &fakePRs{list: []domain.PullRequest{testPR()}, pr: testPR()}
	creator := &fakeCreator{out: domain.PullRequest{Number: 12}}
	ts, client, _, csrf := newCreateTestServerWithPRs(t, prs, creator)

	// GET on the collection still lists.
	resp := get(t, createURL(ts, "proj-1"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", resp.StatusCode)
	}
	got := decode[[]prPayload](t, resp)
	if len(got) != 1 {
		t.Fatalf("list = %+v, want one row", got)
	}
	if creator.calls != 0 {
		t.Fatalf("GET reached the create command")
	}
	// GET on the detail route still reads one.
	resp2 := get(t, createURL(ts, "proj-1")+"/7")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("detail status = %d, want 200", resp2.StatusCode)
	}
	// POST on a non-numeric detail path still names nothing (the create route
	// is the COLLECTION, never a longer path).
	resp3 := createPost(t, client, createURL(ts, "proj-1")+"/7", csrf, createKey, createBody)
	defer resp3.Body.Close()
	if resp3.StatusCode == http.StatusCreated {
		t.Fatalf("a POST to /pull-requests/7 opened a proposal")
	}
	if creator.calls != 0 {
		t.Fatalf("the create command was reached by a detail-path POST")
	}
}
