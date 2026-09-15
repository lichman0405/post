package reviewhttp

import (
	"context"
	"net/http"

	"github.com/lichman0405/post/internal/application/reviews"
	"github.com/lichman0405/post/internal/domain"
)

// Service is the application surface the handler calls: the review
// submission command. The production implementation is *reviews.Service.
type Service interface {
	SubmitReview(ctx context.Context, actor domain.User, projectID string, number int64, in reviews.SubmitReviewInput) (domain.Review, error)
}

// Deps carries the service the surface calls. The production wiring is in
// cmd/api/main.go: the reviews service composed over the review store,
// the projects membership gate and the matrix engine (the reviewer-
// responsibility hook stays nil until T0604 lands the resolver, so the
// conditional verdict fails closed in production).
type Deps struct {
	Service Service
}

// New wires the handler.
func New(deps Deps) *API {
	return &API{handlers: &handlers{svc: deps.Service}}
}

// API is the mounted review surface.
type API struct {
	handlers *handlers
}

// Register mounts the review routes on the guarded v1 mux.
func (a *API) Register(v1 *http.ServeMux) {
	v1.HandleFunc("POST /api/v1/projects/{projectId}/pull-requests/{prId}/reviews", a.handlers.handleSubmitReview)
}
