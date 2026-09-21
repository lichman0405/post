package pullrequestshttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/forks"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/domain"
)

// The request-review tests prove the transport for
// POST /api/v1/projects/{projectId}/pull-requests/{prId}:request-review: the
// contract's path, its required Idempotency-Key, the real guard in front of it
// (session + CSRF, as cmd/api composes it), and the mapping of the forks and
// pullrequests packages' outcomes onto statuses. The suffix split is exercised
// here too, because the handler is mounted at the same pattern production
// mounts it at — the prefix's remainder wildcard, since ServeMux cannot express
// a suffix inside a segment (see requestreview.go).
//
// What these tests do NOT own: the authorization rules (internal/application/
// forks/service_test.go resolves the open_pr cell against real projects,
// memberships and lineage) and the state move itself
// (tests/integration/request_review_route_test.go drives the whole route over
// real PostgreSQL and asserts the stored row).
const reviewTestProjectID = "11111111-2222-4333-8444-555555555556"

// reviewTestKey is a well-formed Idempotency-Key (>= MinCreationKeyLen).
const reviewTestKey = "pr-review-00000001"

// fakeReviewer is the RequestReviewer: it records the actor and the request the
// handler passed it and answers a canned outcome.
type fakeReviewer struct {
	out      domain.PullRequest
	err      error
	calls    int
	gotActor forks.Actor
	gotIn    forks.RequestReviewRequest
}

func (f *fakeReviewer) RequestReview(_ context.Context, actor forks.Actor, in forks.RequestReviewRequest) (domain.PullRequest, error) {
	f.calls++
	f.gotActor, f.gotIn = actor, in
	return f.out, f.err
}

// newReviewTestServer composes the auth surface + the pull-request surface the
// way cmd/api does (the guard wraps the v1 subtree, so the session cookie and
// the CSRF token are the production ones) and mounts the request-review handler
// at the pattern the route is served at: this collection's last segment as a
// remainder wildcard, which is where cmd/api/mergehttp dispatches the
// ":request-review" verb to this handler from.
func newReviewTestServer(t *testing.T, review RequestReviewer) (ts *httptest.Server, client *http.Client, actorID, csrf string) {
	t.Helper()
	authAPI := memstoreAuthAPI(t)
	api := New(Deps{PullRequests: &fakePRs{}, Review: review, Projects: &fakeProjectGate{}})
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	mux.HandleFunc("POST /api/v1/projects/{projectId}/pull-requests/{number...}", api.RequestReviewHandler())
	ts = httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)
	client, actorID, csrf = signupOn(t, ts)
	return ts, client, actorID, csrf
}

// reviewPost performs one request-review write. A caller with no session uses
// http.DefaultClient (the guard refuses it before routing).
func reviewPost(t *testing.T, client *http.Client, url, csrf, key string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, nil)
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

// reviewURL is the CONTRACT's path (specs/api/openapi.yaml:
// /projects/{projectId}/pull-requests/{prId}:request-review), spelled with the
// verb inside the last segment.
func reviewURL(server *httptest.Server, projectID string, number string) string {
	return server.URL + "/api/v1/projects/" + projectID + "/pull-requests/" + number + RequestReviewVerb
}

// TestRequestReviewRouteMovesTheProposalOnTheContractPath: the endpoint the
// contract defines answers 200 with the proposal it moved, and the command
// received the authenticated actor, the project and the number from the PATH
// and the key from the header.
func TestRequestReviewRouteMovesTheProposalOnTheContractPath(t *testing.T) {
	reviewer := &fakeReviewer{out: domain.PullRequest{
		ID: "pr-9", ProjectID: reviewTestProjectID, Number: 12,
		SourceBranchID: "branch-src", TargetBranchID: "branch-main",
		BaseStateID: "state-main", ProposedStateID: "state-src",
		Title: "annealing protocol", Body: "the evidence is on the branch",
		State: domain.PullRequestStateReviewRequired, CreatedBy: "someone",
	}}
	ts, client, actorID, csrf := newReviewTestServer(t, reviewer)

	resp := reviewPost(t, client, reviewURL(ts, reviewTestProjectID, "12"), csrf, reviewTestKey)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := decode[prPayload](t, resp)
	if got.Number != 12 || got.State != string(domain.PullRequestStateReviewRequired) {
		t.Fatalf("payload = %+v, want the proposal in review", got)
	}
	if got.ID != "pr-9" || got.ProposedStateID != "state-src" {
		t.Fatalf("payload = %+v, want the pull-request document", got)
	}
	if reviewer.calls != 1 {
		t.Fatalf("command called %d times, want 1", reviewer.calls)
	}
	if reviewer.gotActor.User.ID != actorID || reviewer.gotActor.User.ID == "" {
		t.Fatalf("command actor = %q, want the guard's %q", reviewer.gotActor.User.ID, actorID)
	}
	// The session edge resolves no agent flag, so the command is told what
	// this build knows (see the handler: a statement about the build, not
	// about the matrix's agent column).
	if reviewer.gotActor.IsAgent {
		t.Fatalf("command actor arrived as an agent on a session route")
	}
	if reviewer.gotIn.ProjectID != reviewTestProjectID {
		t.Fatalf("command project = %q, want the path's %q", reviewer.gotIn.ProjectID, reviewTestProjectID)
	}
	if reviewer.gotIn.Number != 12 {
		t.Fatalf("command number = %d, want 12 (parsed off the verb suffix)", reviewer.gotIn.Number)
	}
	if reviewer.gotIn.IdempotencyKey != reviewTestKey {
		t.Fatalf("command key = %q, want %q", reviewer.gotIn.IdempotencyKey, reviewTestKey)
	}
}

// TestRequestReviewRouteRequiresASession: the guard refuses an anonymous write
// before routing — a caller without a session never reaches the command.
func TestRequestReviewRouteRequiresASession(t *testing.T) {
	reviewer := &fakeReviewer{}
	ts, _, _, _ := newReviewTestServer(t, reviewer)

	resp := reviewPost(t, http.DefaultClient, reviewURL(ts, reviewTestProjectID, "12"), "", reviewTestKey)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if reviewer.calls != 0 {
		t.Fatalf("command reached %d times without a session", reviewer.calls)
	}
}

// TestRequestReviewRouteRequiresTheIdempotencyKey: the route's Idempotency-Key
// is required by the contract (components.parameters.IdempotencyKey, the same
// parameter the merge route carries) and the handler enforces it in place — a
// request that cannot carry a key is refused, never sent without one.
func TestRequestReviewRouteRequiresTheIdempotencyKey(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		reviewer := &fakeReviewer{}
		ts, client, _, csrf := newReviewTestServer(t, reviewer)
		resp := reviewPost(t, client, reviewURL(ts, reviewTestProjectID, "12"), csrf, "")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		if got := decode[errorEnvelope](t, resp); got.Code != pullrequests.CodeValidation {
			t.Fatalf("code = %q, want %q", got.Code, pullrequests.CodeValidation)
		}
		if reviewer.calls != 0 {
			t.Fatalf("command reached without a key")
		}
	})
	t.Run("too short", func(t *testing.T) {
		reviewer := &fakeReviewer{}
		ts, client, _, csrf := newReviewTestServer(t, reviewer)
		short := strings.Repeat("k", pullrequests.MinCreationKeyLen-1)
		resp := reviewPost(t, client, reviewURL(ts, reviewTestProjectID, "12"), csrf, short)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		if reviewer.calls != 0 {
			t.Fatalf("command reached with a key the contract refuses")
		}
	})
	t.Run("a session without the CSRF token is refused", func(t *testing.T) {
		reviewer := &fakeReviewer{}
		ts, client, _, _ := newReviewTestServer(t, reviewer)
		resp := reviewPost(t, client, reviewURL(ts, reviewTestProjectID, "12"), "", reviewTestKey)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (the v1 write guard's CSRF refusal)", resp.StatusCode)
		}
		if reviewer.calls != 0 {
			t.Fatalf("command reached without the session-bound CSRF token")
		}
	})
}

// TestRequestReviewRouteAddressesNothingWithoutItsSuffix: the pattern is a
// remainder wildcard, so a POST to a path with the same prefix and no verb
// reaches the handler — and is answered 404, the same way the merge route
// answers a segment it cannot read as its own verb. A malformed number IS a
// 400: the suffix makes the intent unambiguous.
func TestRequestReviewRouteAddressesNothingWithoutItsSuffix(t *testing.T) {
	t.Run("no suffix", func(t *testing.T) {
		reviewer := &fakeReviewer{}
		ts, client, _, csrf := newReviewTestServer(t, reviewer)
		resp := reviewPost(t, client, ts.URL+"/api/v1/projects/"+reviewTestProjectID+"/pull-requests/12", csrf, reviewTestKey)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		if reviewer.calls != 0 {
			t.Fatalf("command reached from a path that names no verb")
		}
	})
	t.Run("non-numeric number", func(t *testing.T) {
		reviewer := &fakeReviewer{}
		ts, client, _, csrf := newReviewTestServer(t, reviewer)
		resp := reviewPost(t, client, reviewURL(ts, reviewTestProjectID, "abc"), csrf, reviewTestKey)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		if got := decode[errorEnvelope](t, resp); got.Code != pullrequests.CodeValidation {
			t.Fatalf("code = %q, want %q", got.Code, pullrequests.CodeValidation)
		}
		if reviewer.calls != 0 {
			t.Fatalf("command reached with a number that is not one")
		}
	})
}

// TestRequestReviewRouteOutcomes: one outcome, one status and one stable code
// (docs/45), whichever layer reported it — the matrix refusal, an unreadable
// project, a proposal that is not there, and the three state refusals the
// command raises.
func TestRequestReviewRouteOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{
			name:   "a denied cell",
			err:    forks.ErrForbidden,
			status: http.StatusForbidden,
			code:   codeForbidden,
		},
		{
			name:   "a project the caller may not read",
			err:    forks.ErrProjectNotFound,
			status: http.StatusNotFound,
			code:   projects.CodeProjectNotFound,
		},
		{
			name:   "no such proposal",
			err:    pullrequests.ErrPullRequestNotFound,
			status: http.StatusNotFound,
			code:   pullrequests.CodePullRequestNotFound,
		},
		{
			name:   "an edge docs/43 does not have",
			err:    &pullrequests.TransitionError{Number: 12, From: domain.PullRequestStateApproved, To: domain.PullRequestStateReviewRequired},
			status: http.StatusConflict,
			code:   pullrequests.CodeInvalidTransition,
		},
		{
			// A terminal proposal is refused as the edge refusal it is, not
			// as a pullrequests.TerminalError: the command answers
			// *TransitionError for merged/closed/aborted exactly as its
			// setState siblings do (internal/persistence/pullrequest_store.go),
			// so CodeTerminal is NOT an outcome this route can receive and
			// this table does not claim it is.
			name:   "a terminal proposal",
			err:    &pullrequests.TransitionError{Number: 12, From: domain.PullRequestStateMerged, To: domain.PullRequestStateReviewRequired},
			status: http.StatusConflict,
			code:   pullrequests.CodeInvalidTransition,
		},
		{
			name:   "a lost compare-and-swap",
			err:    &pullrequests.StateConflictError{Number: 12, Current: domain.PullRequestStateApproved},
			status: http.StatusConflict,
			code:   pullrequests.CodeStateConflict,
		},
		{
			name:   "a malformed address",
			err:    forks.ErrValidation,
			status: http.StatusBadRequest,
			code:   pullrequests.CodeValidation,
		},
		{
			name:   "the store could not be reached",
			err:    forks.ErrStore,
			status: http.StatusServiceUnavailable,
			code:   pullrequests.CodeUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reviewer := &fakeReviewer{err: tc.err}
			ts, client, _, csrf := newReviewTestServer(t, reviewer)
			resp := reviewPost(t, client, reviewURL(ts, reviewTestProjectID, "12"), csrf, reviewTestKey)
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			got := decode[errorEnvelope](t, resp)
			if got.Code != tc.code {
				t.Fatalf("code = %q, want %q", got.Code, tc.code)
			}
			// The wire line is the surface's own, never err.Error(): a
			// package's prefix and its internals belong in the log (docs/45).
			if strings.Contains(got.Message, "forks:") || strings.Contains(got.Message, "pullrequests:") {
				t.Fatalf("message leaks the package prefix: %q", got.Message)
			}
		})
	}
}

// TestRequestReviewRouteFailsClosedWithoutItsCommand: the wiring is required
// for this verb. A surface built without it refuses the write rather than
// moving any state with no authorization behind it.
func TestRequestReviewRouteFailsClosedWithoutItsCommand(t *testing.T) {
	authAPI := memstoreAuthAPI(t)
	api := New(Deps{PullRequests: &fakePRs{}, Projects: &fakeProjectGate{}})
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	mux.HandleFunc("POST /api/v1/projects/{projectId}/pull-requests/{number...}", api.RequestReviewHandler())
	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)
	client, _, csrf := signupOn(t, ts)

	resp := reviewPost(t, client, reviewURL(ts, reviewTestProjectID, "12"), csrf, reviewTestKey)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := decode[errorEnvelope](t, resp); got.Code != pullrequests.CodeUnavailable {
		t.Fatalf("code = %q, want %q", got.Code, pullrequests.CodeUnavailable)
	}
}

// TestRequestReviewRouteIsTheRouteTheContractDeclares: the contract declares
// this path with the POST verb, and the two outcomes its prose promises — the
// move (200) and the state refusal (409) — are statuses this transport really
// answers.
//
// The comparison is one direction only, and deliberately so: the contract's
// entry for this route declares 200 and 409 (the same minimal shape its entry
// for .../{prId}:merge has), while the transport also answers the collection's
// shared vocabulary — 400 for a malformed address or a missing key, 401, 403
// for a denied cell, 404 for a project or proposal the caller may not address,
// 503 for a surface that could not run. Asserting equality here would fail
// against the landed contract, and asserting a clean set would hide that the
// entry names fewer answers than the route gives. What this test does claim is
// checkable: the path and verb are declared, and both statuses the contract
// promises are reachable through the real handler.
func TestRequestReviewRouteIsTheRouteTheContractDeclares(t *testing.T) {
	contract := loadContract(t)
	contract.operation(t, contractPRPath+"/{prId}:request-review", "post")

	moved := &fakeReviewer{out: domain.PullRequest{Number: 12, State: domain.PullRequestStateReviewRequired}}
	ts, client, _, csrf := newReviewTestServer(t, moved)
	resp := reviewPost(t, client, reviewURL(ts, reviewTestProjectID, "12"), csrf, reviewTestKey)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the contract's 200 for a proposal sent to review", resp.StatusCode)
	}

	refused := &fakeReviewer{err: &pullrequests.TransitionError{
		Number: 12, From: domain.PullRequestStateApproved, To: domain.PullRequestStateReviewRequired,
	}}
	ts2, client2, _, csrf2 := newReviewTestServer(t, refused)
	resp2 := reviewPost(t, client2, reviewURL(ts2, reviewTestProjectID, "12"), csrf2, reviewTestKey)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want the contract's 409 for a state that cannot be sent to review", resp2.StatusCode)
	}
}

// TestRequestReviewVerbIsTheContractSuffix: the literal cmd/api/mergehttp
// dispatches on and this package splits off is the contract's, spelled the way
// specs/api/openapi.yaml spells it. It is exported because two packages read
// it (see requestreview.go), and this test is what keeps the contract's path,
// the dispatcher and the parser one string.
func TestRequestReviewVerbIsTheContractSuffix(t *testing.T) {
	if RequestReviewVerb != ":request-review" {
		t.Fatalf("RequestReviewVerb = %q", RequestReviewVerb)
	}
	contract := loadContract(t)
	if _, ok := contract.Paths[contractPRPath+"/{prId}"+RequestReviewVerb]; !ok {
		t.Fatalf("the contract declares no path %q", contractPRPath+"/{prId}"+RequestReviewVerb)
	}
}
