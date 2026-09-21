package pullrequestshttp

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/forks"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/domain"
)

// RequestReviewVerb is the literal suffix the contract's request-review path
// carries inside its last segment (specs/api/openapi.yaml:
// /projects/{projectId}/pull-requests/{prId}:request-review).
//
// It is exported because the suffix has TWO readers and they must not drift:
// this package splits the number off it, and cmd/api/mergehttp dispatches the
// segment to this package by it (RequestReviewHandler below).
const RequestReviewVerb = ":request-review"

// RequestReviewer is the review-request command:
// POST /api/v1/projects/{projectId}/pull-requests/{number}:request-review.
//
// As with PRCreator, the port is deliberately NOT pullrequests.Service: the
// move needs an authorization decision (it is a governance action), and the
// package that resolves the open_pr cell against a REAL project, a REAL
// membership and — for a non-member — the fork lineage is
// internal/application/forks, whose RequestReview then drives the
// pull-request service. A handler that authorized by itself would be a second,
// drifting copy of the matrix. The production implementation is *forks.Service.
type RequestReviewer interface {
	RequestReview(ctx context.Context, actor forks.Actor, in forks.RequestReviewRequest) (domain.PullRequest, error)
}

// RequestReviewHandler is the request-review route's handler, for the mux that
// registers it to mount.
//
// # Why this package does not register it itself
//
// The contract spells the verb inside the last path segment, and Go's
// ServeMux rejects a wildcard that is not an entire segment
// ("{number}:request-review" panics with "bad wildcard segment"). The route
// therefore has to be captured as a REMAINDER wildcard
// (POST /api/v1/projects/{projectId}/pull-requests/{number...}) and split in a
// handler — and a prefix has exactly ONE remainder owner: registering a second
// remainder pattern under the same prefix panics with "conflicts with
// pattern". That owner is cmd/api/mergehttp (it registered the remainder for
// ":merge" first), so it dispatches this verb here instead of this package
// registering a route the mux would refuse (mergehttp.Deps.ReviewRequest,
// cmd/api/mergehttp/wiring.go).
//
// The handler itself stays in the collection's own package because the answer
// it writes is the pull-request document this surface already renders
// (prPayload): a second rendering of the same resource in another package is a
// second thing to keep in step.
func (a *API) RequestReviewHandler() http.HandlerFunc {
	return a.handlers.handleRequestReview
}

// reviewNumber splits RequestReviewVerb off the route's last segment and parses
// the rest as the PR number. A segment without the suffix is a 404 — the URL
// names nothing (docs/45 existence hiding); a non-numeric number is a 400,
// because the suffix makes the intent unambiguous and the caller is owed the
// part that is wrong.
func reviewNumber(w http.ResponseWriter, r *http.Request, segment string) (int64, bool) {
	if !strings.HasSuffix(segment, RequestReviewVerb) {
		http.NotFound(w, r)
		return 0, false
	}
	raw := strings.TrimSuffix(segment, RequestReviewVerb)
	if raw == "" || strings.Contains(raw, "/") {
		http.NotFound(w, r)
		return 0, false
	}
	number, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || number < 1 {
		authhttp.WriteError(w, r, http.StatusBadRequest, pullrequests.CodeValidation,
			"the pull request number must be a positive integer")
		return 0, false
	}
	return number, true
}

// reviewKey reads the contract-required Idempotency-Key off the header (and
// nowhere else), refusing a missing or too-short one in place (400). The key
// is required here for the same reason it is required on a merge, and the
// promise differs in one respect only: a repeat of a request that already
// carried the proposal into review is answered with the proposal, not with a
// second move (the state is the idempotency record — see the command's doc), so
// a request that cannot carry a key is refused rather than sent without one.
func reviewKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.Header.Get(idempotencyHeader)
	if key == "" {
		authhttp.WriteError(w, r, http.StatusBadRequest, pullrequests.CodeValidation,
			idempotencyHeader+" is required on this route: it is what makes a repeat answer the same way instead of failing the state guard")
		return "", false
	}
	if len(key) < pullrequests.MinCreationKeyLen {
		authhttp.WriteError(w, r, http.StatusBadRequest, pullrequests.CodeValidation,
			idempotencyHeader+" must be at least "+strconv.Itoa(pullrequests.MinCreationKeyLen)+" characters (specs/api/openapi.yaml)")
		return "", false
	}
	return key, true
}

// handleRequestReview: POST
// /api/v1/projects/{projectId}/pull-requests/{number}:request-review.
//
// The move docs/43 names (open → review_required, and changes_requested →
// review_required once the author has answered the requested changes) and the
// only product path into review_required. The contract declares this route and
// its 200/409 answers; the body is the pull-request document this surface
// renders everywhere else.
//
// The project read gate is NOT run here, for the reason handleCreate records:
// the command resolves the caller's visibility itself, and the matrix cell with
// it. A gate in front would either duplicate that resolution or refuse a
// non-member whose fork lineage is exactly what makes the request legitimate —
// and the refusal a caller gets for a project or a proposal they may not act on
// is the same either way (the command resolves authorization before it looks
// at any target).
func (h *handlers) handleRequestReview(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectId")
	number, ok := reviewNumber(w, r, r.PathValue("number"))
	if !ok {
		return
	}
	principal, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		authhttp.WriteError(w, r, http.StatusUnauthorized, "AUTH_UNAUTHENTICATED",
			"authentication required")
		return
	}
	key, ok := reviewKey(w, r)
	if !ok {
		return
	}
	if h.review == nil {
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, pullrequests.CodeUnavailable,
			"pull requests unavailable")
		return
	}
	pr, err := h.review.RequestReview(r.Context(),
		// IsAgent is false, and that is a statement about THIS BUILD rather
		// than about the product rule: a session principal carries no agent
		// flag (the same note aborthttp records at its own Actor
		// construction). The command acts on it, so the day a token edge
		// resolves one, the cell it resolves is the agent's own.
		forks.Actor{User: principal.User, IsAgent: false},
		forks.RequestReviewRequest{
			ProjectID:      projectID,
			Number:         number,
			IdempotencyKey: key,
		})
	if err != nil {
		reviewError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, prToPayload(pr))
}

// reviewError renders one review-request outcome as the standard envelope
// (docs/45: one outcome, one stable code, whichever layer reports it).
func reviewError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, forks.ErrForbidden):
		// The matrix refusal — including the non-member whose proposal is not
		// from their own fork of this project, and the non-member asking about
		// a proposal that does not exist (the command answers the same thing
		// for both, so the refusal reports neither). The code is the one the
		// collection's other routes use for a denied cell.
		authhttp.WriteError(w, r, http.StatusForbidden, codeForbidden,
			"you are not permitted to send this pull request to review")
	case errors.Is(err, forks.ErrProjectNotFound), errors.Is(err, projects.ErrProjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, projects.CodeProjectNotFound,
			"project not found")
	case errors.Is(err, pullrequests.ErrPullRequestNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, pullrequests.CodePullRequestNotFound,
			"pull request not found")
	case errors.Is(err, forks.ErrValidation), errors.Is(err, pullrequests.ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, pullrequests.CodeValidation,
			"invalid project or pull request number")
	default:
		// The two state refusals carry the vocabulary the pull-request surface
		// publishes (docs/45) and the status the contract declares for them
		// ('409': the PR is not in a state that can be sent to review): an edge
		// docs/43 does not have, and a compare-and-swap that lost to a
		// concurrent transition.
		//
		// A terminal proposal is the FIRST of those, not an outcome of its own:
		// the command refuses merged/closed/aborted with the same
		// *TransitionError its setState siblings answer (see
		// internal/persistence/pullrequest_store.go). Mapping a
		// *pullrequests.TerminalError here would be mapping an outcome this
		// route has no way to receive.
		var transition *pullrequests.TransitionError
		var conflict *pullrequests.StateConflictError
		switch {
		case errors.As(err, &transition):
			authhttp.WriteError(w, r, http.StatusConflict, transition.Code(),
				"this pull request cannot be sent to review from the state it is in")
		case errors.As(err, &conflict):
			authhttp.WriteError(w, r, http.StatusConflict, conflict.Code(),
				"the pull request moved underneath this request; re-read it and retry")
		default:
			authhttp.WriteError(w, r, http.StatusServiceUnavailable, pullrequests.CodeUnavailable,
				"pull requests unavailable")
		}
	}
}
