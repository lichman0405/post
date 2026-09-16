package subscriptionshttp

import (
	"net/http"

	"github.com/lichman0405/post/internal/application/subscriptions"
)

// The subscription API wiring: the store adapter goes in, guarded routes
// come out (the guard itself is owned by authhttp and composed in
// cmd/api/main.go). cmd/api builds the production graph
// (events.SubscriptionStore); the integration suite builds the same graph
// over the same real PostgreSQL, so the tests exercise the exact
// production handlers and middleware.

// Deps carries the adapters the subscription API needs.
type Deps struct {
	Store subscriptions.Store
}

// New wires the service.
func New(deps Deps) *API {
	return &API{
		handlers: &handlers{svc: subscriptions.NewService(deps.Store)},
	}
}

// API is the mounted /api/v1/subscriptions subtree.
type API struct {
	handlers *handlers
}

// Service exposes the wired service so sibling surfaces can share the
// exact same instance.
func (a *API) Service() *subscriptions.Service { return a.handlers.svc }

// Routes registers the subscription surface (no guard — main.go wraps the
// whole /api/v1 subtree in authhttp.API.Guard, so every write here is
// session + CSRF protected by construction).
func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	h := a.handlers
	mux.HandleFunc("POST /api/v1/subscriptions", h.handleCreate)
	mux.HandleFunc("GET /api/v1/subscriptions", h.handleList)
	mux.HandleFunc("GET /api/v1/subscriptions/{subscriptionId}", h.handleGet)
	mux.HandleFunc("PATCH /api/v1/subscriptions/{subscriptionId}", h.handleUpdate)
	mux.HandleFunc("DELETE /api/v1/subscriptions/{subscriptionId}", h.handleUnsubscribe)
	return mux
}
