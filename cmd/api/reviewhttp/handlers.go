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

// handlers owns the review route.
type handlers struct {
	svc Service
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
