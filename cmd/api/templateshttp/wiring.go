package templateshttp

import "net/http"

// The template API wiring: the application service goes in, guarded routes
// come out (the guard itself is owned by authhttp and composed in
// cmd/api/main.go). cmd/api builds the production graph — the templates
// service orchestrated over the real projects/schemaprofiles/policy/rsg
// services and the pgx instantiation store; the integration suite builds
// the same graph over the same real PostgreSQL.

// Deps carries the service the routes need.
type Deps struct {
	Service Service
}

// New wires the handlers.
func New(deps Deps) *API {
	return &API{handlers: &handlers{svc: deps.Service}}
}

// API is the mounted template subtree of /api/v1. The wiring site
// (cmd/api/main.go) keeps the application service it built: instantiation
// flows through exactly the same owning-service instances the direct
// routes use, so a project created from a template behaves identically to
// one assembled by hand.
type API struct {
	handlers *handlers
}

// Register mounts the template routes on the v1 mux (no guard — main.go
// wraps the whole /api/v1 subtree in authhttp.API.Guard, so every write
// here is session + CSRF protected by construction). Catalog reads are
// public; the instantiate write resolves its authorization inside the
// projects service's own create_project path.
func (a *API) Register(v1 *http.ServeMux) {
	v1.HandleFunc("GET /api/v1/templates", a.handlers.handleList)
	v1.HandleFunc("GET /api/v1/templates/{templateId}", a.handlers.handleGet)
	v1.HandleFunc("POST /api/v1/templates/{templateId}/instantiate", a.handlers.handleInstantiate)
	v1.HandleFunc("GET /api/v1/projects/{projectId}/template-instantiation", a.handlers.handleGetInstantiation)
}
