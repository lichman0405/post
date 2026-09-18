package inboxhttp

import (
	"net/http"

	"github.com/lichman0405/post/internal/application/inbox"
)

// The inbox API wiring: the store adapter goes in, guarded routes come out
// (the guard itself is owned by authhttp and composed in cmd/api/main.go).
// cmd/api builds the production graph (events.SubscriptionStore); the
// integration suite builds the same graph over the same real PostgreSQL,
// so the tests exercise the exact production handlers and middleware.

// Deps carries the adapters the inbox API needs.
type Deps struct {
	Store inbox.Store
}

// New wires the service.
func New(deps Deps) *API {
	return &API{handlers: &handlers{svc: inbox.NewService(deps.Store)}}
}

// API is the mounted /api/v1/inbox subtree.
type API struct {
	handlers *handlers
}

// Service exposes the wired service so sibling surfaces can share the
// exact same instance.
func (a *API) Service() *inbox.Service { return a.handlers.svc }

// Routes registers the inbox surface (no guard — main.go wraps the whole
// /api/v1 subtree in authhttp.API.Guard, so every write here is session +
// CSRF protected by construction).
func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	h := a.handlers
	mux.HandleFunc("GET /api/v1/inbox", h.handleList)
	mux.HandleFunc("POST /api/v1/inbox/read", h.handleMarkRead)
	mux.HandleFunc("POST /api/v1/inbox/read-all", h.handleMarkAllRead)
	return mux
}
