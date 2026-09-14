package releasehttp

import (
	"net/http"
)

// The release API wiring: the release command (T0606) and the project
// read gate go in, guarded routes come out (the guard itself is owned by
// authhttp and composed in cmd/api/main.go). cmd/api builds the
// production graph; the integration suite builds the same graph over the
// same real PostgreSQL.

// Deps carries the command and the visibility gate the release API
// needs.
type Deps struct {
	// Command is the wired release command (CommandPort; the production
	// value is *releases.Command).
	Command CommandPort
	// Projects is the project read gate: a release is exactly as
	// visible as its project. The production adapter is
	// projectAPI.Service().
	Projects ProjectVisibilityPort
}

// New wires the handlers.
func New(deps Deps) *API {
	return &API{
		handlers: &handlers{cmd: deps.Command, projects: deps.Projects},
	}
}

// API is the mounted release surface.
type API struct {
	handlers *handlers
}

// Register mounts the release routes directly on the v1 mux. The
// full-path patterns are more specific than the /api/v1/projects/
// subtree, so they coexist with it (same technique as policyhttp).
// There is deliberately no PUT/PATCH/DELETE registration: a release is
// immutable — the surface has no edit and no delete route to call. On a
// bare mux the method patterns make the mux answer 405 for mutating
// methods; in the composed tree (with the projects subtree registered)
// such a request falls through to the subtree's own "no such route" 404
// — either way the release command never runs, and the append-only
// database guard is the final backstop (migration 00014).
func (a *API) Register(v1 *http.ServeMux) {
	h := a.handlers
	v1.HandleFunc("GET /api/v1/projects/{projectId}/releases", h.handleListReleases)
	v1.HandleFunc("POST /api/v1/projects/{projectId}/releases", h.handleCreateRelease)
	v1.HandleFunc("GET /api/v1/projects/{projectId}/releases/{releaseId}", h.handleGetRelease)
	v1.HandleFunc("GET /api/v1/projects/{projectId}/releases/{releaseId}/manifest", h.handleGetReleaseManifest)
}
