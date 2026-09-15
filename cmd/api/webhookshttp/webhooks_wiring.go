package webhookshttp

import (
	"net/http"

	"github.com/lichman0405/post/internal/application/webhooks"
)

// The webhook API wiring: the store adapter goes in, guarded routes come
// out (the guard itself is owned by authhttp and composed in
// cmd/api/main.go). cmd/api builds the production graph (events.
// WebhookStore); the integration suite builds the same graph over the
// same real PostgreSQL, so the tests exercise the exact production
// handlers and middleware.

// Deps carries the adapters the webhook API needs.
type Deps struct {
	Store webhooks.Store
}

// New wires the service.
func New(deps Deps) *API {
	return &API{
		handlers: &handlers{svc: webhooks.NewService(deps.Store)},
	}
}

// API is the mounted /api/v1/webhooks subtree.
type API struct {
	handlers *handlers
}

// Service exposes the wired service so sibling surfaces can share the
// exact same instance.
func (a *API) Service() *webhooks.Service { return a.handlers.svc }

// Routes registers the webhook surface (no guard — main.go wraps the
// whole /api/v1 subtree in authhttp.API.Guard, so every write here is
// session + CSRF protected by construction).
func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	h := a.handlers
	mux.HandleFunc("POST /api/v1/webhooks", h.handleCreate)
	mux.HandleFunc("GET /api/v1/webhooks", h.handleList)
	mux.HandleFunc("GET /api/v1/webhooks/{webhookId}", h.handleGet)
	mux.HandleFunc("PATCH /api/v1/webhooks/{webhookId}", h.handleUpdate)
	mux.HandleFunc("POST /api/v1/webhooks/{webhookId}/secret", h.handleRotateSecret)
	mux.HandleFunc("DELETE /api/v1/webhooks/{webhookId}", h.handleDelete)
	mux.HandleFunc("GET /api/v1/webhooks/{webhookId}/deliveries", h.handleListDeliveries)
	mux.HandleFunc("POST /api/v1/webhooks/{webhookId}/deliveries/{deliveryId}/retry", h.handleRedeliver)
	return mux
}
