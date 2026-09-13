package audithttp

import (
	"net/http"

	"github.com/lichman0405/post/internal/application/audit"
)

// The Activity API wiring: the read store and the two read gates go in,
// GET-only routes come out (the guard is owned by authhttp and composed in
// cmd/api/main.go; every route here inherits it structurally).

// Deps carries the adapters the Activity API needs.
type Deps struct {
	Store audit.Store
	// Projects is the project read gate: the same member-only,
	// existence-hiding rule as the project surface. Production composes
	// the same *projects.Service the projectshttp surface uses.
	Projects audit.ProjectReadGate
	// Orgs is the organization read gate, likewise shared with the orgs
	// surface.
	Orgs audit.OrgReadGate
}

// New wires the service.
func New(deps Deps) *API {
	return &API{
		handlers: &handlers{svc: audit.NewService(deps.Store, deps.Projects, deps.Orgs)},
	}
}

// API registers the Activity routes on the shared /api/v1 mux.
type API struct {
	handlers *handlers
}

// Register mounts the read-only Activity surface directly on the v1 mux
// (the full-path patterns are more specific than the /api/v1/projects/
// and /api/v1/organizations/ subtrees, so they coexist with them). No
// state-changing verb has a route; the handlers answer 405 for anything
// but GET — the log has no update/delete surface, and the routing answer
// says so explicitly instead of falling through to a subtree 404.
func (a *API) Register(v1 *http.ServeMux) {
	v1.HandleFunc("/api/v1/projects/{projectId}/activity", a.handlers.handleProjectActivity)
	v1.HandleFunc("/api/v1/organizations/{orgId}/activity", a.handlers.handleOrgActivity)
}
