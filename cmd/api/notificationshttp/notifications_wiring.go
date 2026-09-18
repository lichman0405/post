package notificationshttp

import (
	"net/http"

	"github.com/lichman0405/post/internal/application/notifications"
)

// The notification-settings API wiring: the store adapter goes in, guarded
// routes come out (the guard itself is owned by authhttp and composed in
// cmd/api/main.go). cmd/api builds the production graph
// (events.NotificationStore over the live pool); the integration suite
// builds the same graph over the same real PostgreSQL, so the tests exercise
// the exact production handlers.

// Deps carries the adapters the settings API needs.
type Deps struct {
	Store notifications.Store
}

// New wires the service.
func New(deps Deps) *API {
	return &API{handlers: &handlers{svc: notifications.NewService(deps.Store)}}
}

// API is the mounted /api/v1/notifications subtree.
type API struct {
	handlers *handlers
}

// Service exposes the wired service so sibling surfaces can share the exact
// same instance.
func (a *API) Service() *notifications.Service { return a.handlers.svc }

// Routes registers the notification-settings surface (no guard — main.go
// wraps the whole /api/v1 subtree in authhttp.API.Guard, so both routes
// here are session + CSRF protected by construction).
func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	h := a.handlers
	mux.HandleFunc("GET /api/v1/notifications/preferences", h.handleGetPreferences)
	mux.HandleFunc("PUT /api/v1/notifications/preferences", h.handleSetCadence)
	return mux
}
