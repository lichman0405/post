package orgshttp

import (
	"net/http"

	"github.com/lichman0405/post/internal/application/orgs"
)

// The org API wiring: the store adapter goes in, guarded routes come out
// (the guard itself is owned by authhttp and composed in cmd/api/main.go).
// cmd/api builds the production graph (pgx OrgStore); the integration
// suite builds the same graph over the same real PostgreSQL, so the tests
// exercise the exact production handlers and middleware.

// Deps carries the adapters the org API needs.
type Deps struct {
	Store orgs.OrgStore
}

// New wires the service.
func New(deps Deps) *API {
	return &API{
		handlers: &handlers{svc: orgs.NewService(deps.Store)},
	}
}

// API is the mounted /api/v1/organizations subtree.
type API struct {
	handlers *handlers
}

// Routes registers the organization surface (no guard — main.go wraps the
// whole /api/v1 subtree in authhttp.API.Guard, so every write here is
// session + CSRF protected by construction).
func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	h := a.handlers
	mux.HandleFunc("POST /api/v1/organizations", h.handleCreate)
	mux.HandleFunc("GET /api/v1/organizations", h.handleList)
	mux.HandleFunc("GET /api/v1/organizations/{orgId}", h.handleGet)
	mux.HandleFunc("PATCH /api/v1/organizations/{orgId}", h.handleUpdate)
	mux.HandleFunc("DELETE /api/v1/organizations/{orgId}", h.handleDeactivate)
	mux.HandleFunc("GET /api/v1/organizations/{orgId}/members", h.handleListMembers)
	mux.HandleFunc("POST /api/v1/organizations/{orgId}/members", h.handleInvite)
	mux.HandleFunc("PATCH /api/v1/organizations/{orgId}/members/{userId}", h.handleUpdateMembership)
	mux.HandleFunc("DELETE /api/v1/organizations/{orgId}/members/{userId}", h.handleRemoveMember)
	return mux
}
