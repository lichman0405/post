package schemaprofileshttp

import "net/http"

// The schema profile API wiring: the application service goes in, guarded
// routes come out (the guard itself is owned by authhttp and composed in
// cmd/api/main.go). cmd/api builds the production graph (pgx store + the
// project surface + the schema registry); the integration suite builds the
// same graph over the same real PostgreSQL.

// Deps carries the service the routes need.
type Deps struct {
	Service Service
}

// New wires the handlers.
func New(deps Deps) *API {
	return &API{handlers: &handlers{svc: deps.Service}}
}

// API is the mounted schema-profiles subtree of /api/v1/projects. The
// wiring site (cmd/api/main.go) keeps the application service it built and
// shares the same instance with the RSG service — the object create path
// resolves schema refs against exactly the profiles these routes register.
type API struct {
	handlers *handlers
}

// Register mounts the schema profile routes on the v1 mux (no guard —
// main.go wraps the whole /api/v1 subtree in authhttp.API.Guard, so every
// write here is session + CSRF protected by construction).
func (a *API) Register(v1 *http.ServeMux) {
	v1.HandleFunc("POST /api/v1/projects/{projectId}/schema-profiles", a.handlers.handleRegister)
	v1.HandleFunc("GET /api/v1/projects/{projectId}/schema-profiles", a.handlers.handleList)
	v1.HandleFunc("GET /api/v1/projects/{projectId}/schema-profiles/{profileName}", a.handlers.handleGet)
	v1.HandleFunc("GET /api/v1/projects/{projectId}/schema-profiles/{profileName}/versions/{version}", a.handlers.handleGetVersion)
}
