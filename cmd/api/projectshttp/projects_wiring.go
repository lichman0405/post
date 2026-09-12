package projectshttp

import (
	"net/http"

	"github.com/lichman0405/post/internal/application/projects"
)

// The project API wiring: the store adapter and the organization gate go
// in, guarded routes come out (the guard itself is owned by authhttp and
// composed in cmd/api/main.go). cmd/api builds the production graph (pgx
// ProjectStore + pgx OrgStore as the gate); the integration suite builds
// the same graph over the same real PostgreSQL, so the tests exercise the
// exact production handlers and middleware.

// Deps carries the adapters the project API needs.
type Deps struct {
	Store projects.ProjectStore
	// Orgs is the organization gate: create-inside-organization checks
	// (organization exists, active, actor is an active member). The
	// production adapter is persistence.OrgStore.
	Orgs projects.OrgGate
}

// New wires the service.
func New(deps Deps) *API {
	return &API{
		handlers: &handlers{svc: projects.NewService(deps.Store, deps.Orgs)},
	}
}

// API is the mounted /api/v1/projects subtree.
type API struct {
	handlers *handlers
}

// Routes registers the project surface (no guard — main.go wraps the whole
// /api/v1 subtree in authhttp.API.Guard, so every write here is session +
// CSRF protected by construction).
func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	h := a.handlers
	mux.HandleFunc("POST /api/v1/projects", h.handleCreate)
	mux.HandleFunc("GET /api/v1/projects", h.handleList)
	mux.HandleFunc("GET /api/v1/projects/{projectId}", h.handleGet)
	return mux
}
