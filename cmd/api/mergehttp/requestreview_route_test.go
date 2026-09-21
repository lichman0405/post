package mergehttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lichman0405/post/cmd/api/pullrequestshttp"
	"github.com/lichman0405/post/internal/application/forks"
	"github.com/lichman0405/post/internal/domain"
)

// The dispatch tests for this collection's last path segment.
//
// Two verbs of specs/api/openapi.yaml live inside that segment —
// "/pull-requests/{prId}:merge" and "/pull-requests/{prId}:request-review" —
// and Go's ServeMux can express neither as a pattern (a wildcard that is not a
// whole segment is "bad wildcard segment"), so both are captured by ONE
// remainder wildcard. A prefix has exactly one remainder owner (a second
// registration panics with "conflicts with pattern"), so this package — which
// registered the remainder for ":merge" first — is the dispatcher for the whole
// segment, and ":request-review" is served by the surface that owns the
// collection (Deps.ReviewRequest, the production value being
// pullrequestshttp.API.RequestReviewHandler).
//
// These tests are what keeps that arrangement honest: the segment must still
// reach the merge command for ":merge", reach the other surface for
// ":request-review", reach the review-submission route when the path has one
// more segment, and reach NOTHING for any other suffix.
//
// The production composition is used, not an imitation of it: the second
// surface is built with pullrequestshttp.New + RequestReviewHandler, the very
// call cmd/api/main.go makes.

const dispatchProjectID = "11111111-2222-4333-8444-555555555555"

// reviewStub is the second surface's command: it records that the dispatched
// verb reached it.
type reviewStub struct {
	calls int
	actor forks.Actor
	in    forks.RequestReviewRequest
}

func (s *reviewStub) RequestReview(_ context.Context, actor forks.Actor, in forks.RequestReviewRequest) (domain.PullRequest, error) {
	s.calls++
	s.actor, s.in = actor, in
	return domain.PullRequest{
		ID: "pr-1", ProjectID: in.ProjectID, Number: in.Number,
		State: domain.PullRequestStateReviewRequired,
	}, nil
}

// reviewSurface builds the pull-request surface's request-review handler the
// way cmd/api does: the API is constructed with the review command, and the
// handler is what mergehttp dispatches to. The other ports are left nil: the
// review route reaches none of them (a nil one fails closed rather than
// panicking).
func reviewSurface(stub *reviewStub) http.HandlerFunc {
	return pullrequestshttp.New(pullrequestshttp.Deps{Review: stub}).RequestReviewHandler()
}

// dispatchServer composes the merge surface (with or without the second verb
// wired) behind the real guard, and mounts reviewhttp's route beside it — the
// more specific pattern the remainder must not swallow.
func dispatchServer(t *testing.T, deps Deps) (*httptest.Server, *http.Client, string) {
	t.Helper()
	ts, client, _, csrf := newMergeTestServerWithDeps(t, deps)
	return ts, client, csrf
}

// TestPullRequestSegmentDispatchesBothVerbs: one registration, two verbs, and
// each reaches the command that owns it — the merge command for ":merge", the
// pull-request surface's handler for ":request-review". Neither verb reaches
// the other's command.
func TestPullRequestSegmentDispatchesBothVerbs(t *testing.T) {
	t.Run(":merge reaches the merge command", func(t *testing.T) {
		cmd := &fakeCommand{}
		stub := &reviewStub{}
		ts, client, csrf := dispatchServer(t, Deps{Command: cmd, Projects: &fakeGate{}, ReviewRequest: reviewSurface(stub)})

		resp := mergePost(t, client, mergeURL(ts, "7"), csrf, mergeTestKey, "")
		status, _, _ := envelope(t, resp)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if cmd.calls != 1 {
			t.Fatalf("merge command called %d times, want 1", cmd.calls)
		}
		if stub.calls != 0 {
			t.Fatalf("the OTHER surface's command was reached by a merge: %d calls", stub.calls)
		}
	})
	t.Run(":request-review reaches the pull-request surface", func(t *testing.T) {
		cmd := &fakeCommand{}
		stub := &reviewStub{}
		ts, client, csrf := dispatchServer(t, Deps{Command: cmd, Projects: &fakeGate{}, ReviewRequest: reviewSurface(stub)})

		resp := mergePost(t, client, ts.URL+"/api/v1/projects/"+dispatchProjectID+"/pull-requests/7"+pullrequestshttp.RequestReviewVerb,
			csrf, mergeTestKey, "")
		status, body, _ := envelope(t, resp)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", status, body)
		}
		if stub.calls != 1 {
			t.Fatalf("review command called %d times, want 1", stub.calls)
		}
		if stub.in.ProjectID != dispatchProjectID || stub.in.Number != 7 {
			t.Fatalf("review command got project %q number %d, want the path's", stub.in.ProjectID, stub.in.Number)
		}
		if stub.in.IdempotencyKey != mergeTestKey {
			t.Fatalf("review command got key %q, want %q", stub.in.IdempotencyKey, mergeTestKey)
		}
		if cmd.calls != 0 {
			t.Fatalf("a merge ran for a request-review call: %d merges", cmd.calls)
		}
	})
}

// TestPullRequestSegmentStillAnswersNothingElse: the remainder wildcard reaches
// every path under this prefix, so the dispatch is what keeps an unknown verb
// from becoming a route. A segment that is not one of the two verbs — with or
// without a suffix, numeric or not — is the 404 it has always been, and no
// command is reached.
func TestPullRequestSegmentStillAnswersNothingElse(t *testing.T) {
	for _, number := range []string{"7", "7:merge-ish", "7:request", "7:request-reviewer", "7:"} {
		t.Run(number, func(t *testing.T) {
			cmd := &fakeCommand{}
			stub := &reviewStub{}
			ts, client, csrf := dispatchServer(t, Deps{Command: cmd, Projects: &fakeGate{}, ReviewRequest: reviewSurface(stub)})

			resp := mergePost(t, client, ts.URL+"/api/v1/projects/"+dispatchProjectID+"/pull-requests/"+number, csrf, mergeTestKey, "")
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", resp.StatusCode)
			}
			if cmd.calls != 0 || stub.calls != 0 {
				t.Fatalf("a command was reached from a path that names no verb (merge %d, review %d)", cmd.calls, stub.calls)
			}
		})
	}
}

// TestReviewSubmissionRouteStillWinsOverTheRemainder: "/{number}/reviews" has
// one more segment, so it matches a strict subset of what the remainder
// wildcard matches, and ServeMux picks the more specific pattern — reviewhttp's
// route keeps the review submission, which the remainder must not swallow.
//
// Both patterns are on the same mux here, in the order cmd/api registers them
// (this surface first, reviewhttp second), because that is the arrangement the
// claim is about. reviewhttp's handler is stood in for by a marker: what is
// under test is which pattern wins, not the submission itself.
func TestReviewSubmissionRouteStillWinsOverTheRemainder(t *testing.T) {
	cmd := &fakeCommand{}
	stub := &reviewStub{}
	const reviewsPath = "POST /api/v1/projects/{projectId}/pull-requests/{prId}/reviews"
	ts, client, _, csrf := newMergeTestServerWithDeps(
		t,
		Deps{Command: cmd, Projects: &fakeGate{}, ReviewRequest: reviewSurface(stub)},
		func(mux *http.ServeMux) {
			mux.HandleFunc(reviewsPath, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
			})
		},
	)

	resp := mergePost(t, client, ts.URL+"/api/v1/projects/"+dispatchProjectID+"/pull-requests/7/reviews", csrf, mergeTestKey, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 from the review-submission route (the remainder swallowed it otherwise)", resp.StatusCode)
	}
	if cmd.calls != 0 || stub.calls != 0 {
		t.Fatalf("a command ran for the review-submission path (merge %d, review %d)", cmd.calls, stub.calls)
	}
}

// TestRequestReviewVerbFailsClosedWithoutItsHandler: the verb is optional
// wiring. A mux that mounts the merge surface alone (the E2E merge suite does)
// refuses the verb rather than moving any state, and its own route is
// unaffected.
func TestRequestReviewVerbFailsClosedWithoutItsHandler(t *testing.T) {
	cmd := &fakeCommand{}
	ts, client, csrf := dispatchServer(t, Deps{Command: cmd, Projects: &fakeGate{}})

	resp := mergePost(t, client, ts.URL+"/api/v1/projects/"+dispatchProjectID+"/pull-requests/7"+pullrequestshttp.RequestReviewVerb,
		csrf, mergeTestKey, "")
	status, _, code := envelope(t, resp)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}
	if code == "" {
		t.Fatalf("the refusal carried no wire code")
	}

	// The merge route on the same mux still works: an unwired second verb must
	// not disturb the first.
	resp = mergePost(t, client, mergeURL(ts, "7"), csrf, mergeTestKey, "")
	if status, _, _ := envelope(t, resp); status != http.StatusOK {
		t.Fatalf("merge status = %d, want 200", status)
	}
}
