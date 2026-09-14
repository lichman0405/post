package pullrequestshttp

import (
	"net/http"
)

// Deps carries the adapters the pull-request API needs.
type Deps struct {
	// PullRequests serves the list and detail endpoints (the pullrequests
	// application service).
	PullRequests PullRequests
	// Checks serves the checks endpoint (the prchecks application
	// service).
	Checks CheckRunner
	// Projects must be wired: PRs and their reports are exactly as
	// visible as their project (the same read gate every other project
	// read runs).
	Projects ProjectReader
}

// New wires the handler.
func New(deps Deps) *API {
	return &API{handlers: &handlers{prs: deps.PullRequests, checks: deps.Checks, projects: deps.Projects}}
}

// API is the mounted pull-request surface.
type API struct {
	handlers *handlers
}

// Register mounts the three read endpoints. The checks route registers
// before the {number} detail route: ServeMux matches the most specific
// pattern, but the explicit order documents the intent — "/checks" is
// never consumed as a {number} (which it could not parse anyway).
func (a *API) Register(v1 *http.ServeMux) {
	v1.HandleFunc("GET /api/v1/projects/{projectId}/pull-requests", a.handlers.handleList)
	v1.HandleFunc("GET /api/v1/projects/{projectId}/pull-requests/{number}/checks", a.handlers.handleChecks)
	v1.HandleFunc("GET /api/v1/projects/{projectId}/pull-requests/{number}", a.handlers.handleGet)
}
