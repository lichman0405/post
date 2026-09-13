package projectshttp

import (
	"net/http"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/authz"
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
	// Authz is the policy engine the service enforces through (T0105,
	// docs/50: every public action passes an explicit authorization
	// check — hiding a control in a client never substitutes for it).
	// Production composes authz.NewMatrixEngine().
	Authz authz.Engine
}

// New wires the service.
func New(deps Deps) *API {
	return &API{
		handlers: &handlers{svc: projects.NewService(deps.Store, deps.Orgs, deps.Authz)},
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
	mux.HandleFunc("PATCH /api/v1/projects/{projectId}", h.handleUpdateSettings)
	mux.HandleFunc("GET /api/v1/projects/{projectId}/membership", h.handleMembership)
	mux.HandleFunc("GET /api/v1/projects/{projectId}/members", h.handleListMembers)
	mux.HandleFunc("PUT /api/v1/projects/{projectId}/members/{userId}", h.handleSetMemberRole)
	return mux
}
