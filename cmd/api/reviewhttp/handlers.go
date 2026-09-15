package reviewhttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/application/reviews"
	"github.com/lichman0405/post/internal/domain"
)

// handlers owns the review routes.
type handlers struct {
	svc      Service
	projects ProjectReader
}

// submitReviewRequest is the wire body: the dimension, the verdict and
// the reasoning. The reviewer identity and the responsibility label are
// derived server-side (the guarded principal and the hook) — never taken
// from the client.
type submitReviewRequest struct {
	Kind     string `json:"kind"`
	Decision string `json:"decision"`
	Body     string `json:"body"`
}

// reviewPayload is the client-visible review shape.
type reviewPayload struct {
	ID              string    `json:"id"`
	PullRequestID   string    `json:"pull_request_id"`
	ReviewerID      string    `json:"reviewer_id"`
	Kind            string    `json:"kind"`
	Decision        string    `json:"decision"`
	ReviewedStateID string    `json:"reviewed_state_id"`
	Responsibility  string    `json:"responsibility"`
	Body            string    `json:"body"`
	CreatedAt       time.Time `json:"created_at"`
}

func reviewPayloadFromDomain(r domain.Review) reviewPayload {
	return reviewPayload{
		ID:              r.ID,
		PullRequestID:   r.PullRequestID,
		ReviewerID:      r.ReviewerID,
		Kind:            string(r.Kind),
		Decision:        string(r.Decision),
		ReviewedStateID: r.ReviewedStateID,
		Responsibility:  r.Responsibility,
		Body:            r.Body,
		CreatedAt:       r.CreatedAt,
	}
}

// handleSubmitReview: POST
// /api/v1/projects/{projectId}/pull-requests/{prId}/reviews
func (h *handlers) handleSubmitReview(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	projectID := r.PathValue("projectId")
	number, err := strconv.ParseInt(r.PathValue("prId"), 10, 64)
	if err != nil || number < 1 {
		authhttp.WriteError(w, r, http.StatusBadRequest, reviews.CodeValidation,
			"prId must be a positive pull request number")
		return
	}
	var req submitReviewRequest
	if !decodeBody(w, r, &req) {
		return
	}
	review, err := h.svc.SubmitReview(r.Context(), actor, projectID, number, reviews.SubmitReviewInput{
		Kind:     domain.ReviewKind(req.Kind),
		Decision: domain.ReviewDecision(req.Decision),
		Body:     req.Body,
	})
	if err != nil {
		reviewError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusCreated, reviewPayloadFromDomain(review))
}

// handleListReviews: GET
// /api/v1/projects/{projectId}/pull-requests/{prId}/reviews
//
// The PR's recorded reviews, oldest first (the review section of the PR
// page: both dimensions of the current head, and the earlier rounds'
// decisions, each carrying the state it judged — ReviewedStateID — so a
// reader can tell which head a decision belongs to).
//
// The read runs the project visibility gate first, exactly like the
// pull-request and checks reads: a denied read answers the
// existence-hiding 404, and the service is never reached. A project the
// caller may read but that holds no PR of this number answers an empty
// list — the reviews read is addressed by (project, number), and "no
// reviews recorded" is the same answer for a PR with none (the same read
// discipline as the PR list, whose store returns empty for an unknown
// project).
func (h *handlers) handleListReviews(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectId")
	number, err := strconv.ParseInt(r.PathValue("prId"), 10, 64)
	if err != nil || number < 1 {
		// A malformed segment names nothing: the pull-request surface's
		// own rule for {number} segments is a 404, and the two surfaces
		// address the same resource, so they must agree. (The submission
		// route answers 400 for the same shape — a write with an
		// unusable path is a request the client should fix, and it is the
		// already-released T0404 contract.)
		http.NotFound(w, r)
		return
	}
	if !h.gateProject(w, r, projectID) {
		return
	}
	if h.svc == nil {
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, reviews.CodeUnavailable,
			"review service unavailable")
		return
	}
	list, err := h.svc.List(r.Context(), projectID, number)
	if err != nil {
		switch {
		case errors.Is(err, reviews.ErrValidation):
			authhttp.WriteError(w, r, http.StatusBadRequest, reviews.CodeValidation,
				"invalid project or pull request number")
		default:
			authhttp.WriteError(w, r, http.StatusServiceUnavailable, reviews.CodeUnavailable,
				"review service unavailable")
		}
		return
	}
	out := make([]reviewPayload, 0, len(list))
	for _, review := range list {
		out = append(out, reviewPayloadFromDomain(review))
	}
	authhttp.WriteJSON(w, http.StatusOK, out)
}

// gateProject runs the project read boundary for the list route: a
// denied read — a private project the caller is not a member of —
// answers the same existence-hiding 404 as every other project read.
// The port is required wiring for that route: without it the route fails
// closed.
func (h *handlers) gateProject(w http.ResponseWriter, r *http.Request, projectID string) bool {
	if h.projects == nil {
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, reviews.CodeUnavailable,
			"review service unavailable")
		return false
	}
	if _, err := h.projects.Get(r.Context(), reviewReader(r), projectID); err != nil {
		switch {
		case errors.Is(err, projects.ErrProjectNotFound):
			authhttp.WriteError(w, r, http.StatusNotFound, projects.CodeProjectNotFound,
				"project not found")
		default:
			authhttp.WriteError(w, r, http.StatusServiceUnavailable, reviews.CodeUnavailable,
				"review service unavailable")
		}
		return false
	}
	return true
}

// reviewReader resolves the caller for the project read gate (T0106): a
// session makes them authenticated, its absence makes them anonymous.
func reviewReader(r *http.Request) projects.Reader {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		return projects.Reader{}
	}
	return projects.Reader{UserID: p.User.ID, Authenticated: true}
}

// principal resolves the authenticated actor; a missing session is
// answered in place with 401 (the guard enforces the same before routing
// — this is the handler-level backstop, the same shape rsghttp uses).
func principal(w http.ResponseWriter, r *http.Request) (domain.User, bool) {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		authhttp.WriteError(w, r, http.StatusUnauthorized, authn.CodeUnauthenticated,
			"authentication required")
		return domain.User{}, false
	}
	return p.User, true
}

// decodeBody parses a JSON body (bounded) and reports success — a
// malformed body is answered in place.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if err := dec.Decode(dst); err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, reviews.CodeValidation,
			"request body must be valid JSON")
		return false
	}
	return true
}

// reviewError renders one service outcome as the standard envelope. A
// denied submission answers the same 403 whether or not the PR exists;
// nothing leaks a foreign entity's existence (docs/45).
func reviewError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := reviewErrorOutcome(err)
	authhttp.WriteError(w, r, status, code, message)
}

func reviewErrorOutcome(err error) (status int, code, message string) {
	switch {
	case errors.Is(err, projects.ErrProjectNotFound):
		return http.StatusNotFound, projects.CodeProjectNotFound, "project not found"
	case errors.Is(err, reviews.ErrForbidden):
		return http.StatusForbidden, reviews.CodeForbidden, "you are not permitted to submit a review in this project"
	case errors.Is(err, pullrequests.ErrPullRequestNotFound):
		return http.StatusNotFound, pullrequests.CodePullRequestNotFound, "pull request not found"
	case errors.Is(err, reviews.ErrAlreadyReviewed):
		return http.StatusConflict, reviews.CodeAlreadyReviewed, "you already recorded this review kind for this head — update the proposal and re-request review"
	case errors.As(err, new(*pullrequests.TerminalError)):
		var term *pullrequests.TerminalError
		errors.As(err, &term)
		return http.StatusConflict, pullrequests.CodeTerminal,
			"the pull request is " + string(term.State) + " — a closed proposal accepts no further reviews"
	case errors.Is(err, reviews.ErrValidation):
		return http.StatusBadRequest, reviews.CodeValidation, "validation failed"
	default:
		return http.StatusServiceUnavailable, reviews.CodeUnavailable, "review service unavailable"
	}
}
