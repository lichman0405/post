package reviewhttp

import (
	"context"
	"net/http"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/reviews"
	"github.com/lichman0405/post/internal/domain"
)

// Service is the application surface the handler calls: the review
// submission command and the PR's recorded reviews (the read the PR
// page's review section renders — both review dimensions of the current
// head, plus the earlier rounds' decisions). The production
// implementation is *reviews.Service.
type Service interface {
	SubmitReview(ctx context.Context, actor domain.User, projectID string, number int64, in reviews.SubmitReviewInput) (domain.Review, error)
	List(ctx context.Context, projectID string, number int64) ([]domain.Review, error)
}

// ProjectReader is the visibility gate the list route runs before
// anything else: recorded reviews are exactly as visible as their
// project, the same read gate every other project read runs (T0106 read
// matrix, docs/45). The submission route needs no such gate — the
// reviews service authorizes the principal itself, and a denied caller
// learns nothing about the PR there either way.
type ProjectReader interface {
	Get(ctx context.Context, r projects.Reader, projectID string) (domain.Project, error)
}

// Deps carries the service the surface calls. The production wiring is in
// cmd/api/main.go: the reviews service composed over the review store,
// the projects membership gate and the matrix engine (the reviewer-
// responsibility hook stays nil until T0604 lands the resolver, so the
// conditional verdict fails closed in production).
type Deps struct {
	Service Service
	// Projects is required by the list route; without it that route
	// fails closed (503) rather than serving a review list ungated.
	Projects ProjectReader
}

// New wires the handler.
func New(deps Deps) *API {
	return &API{handlers: &handlers{svc: deps.Service, projects: deps.Projects}}
}

// API is the mounted review surface.
type API struct {
	handlers *handlers
}

// Register mounts the review routes on the guarded v1 mux. The list
// route registers before the {prId} route's siblings for the same reason
// the pull-request surface does: "/reviews" is a sub-resource, never a
// {prId} segment.
func (a *API) Register(v1 *http.ServeMux) {
	v1.HandleFunc("GET /api/v1/projects/{projectId}/pull-requests/{prId}/reviews", a.handlers.handleListReviews)
	v1.HandleFunc("POST /api/v1/projects/{projectId}/pull-requests/{prId}/reviews", a.handlers.handleSubmitReview)
}
