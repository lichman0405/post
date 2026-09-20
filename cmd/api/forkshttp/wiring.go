package forkshttp

import "net/http"

// Deps carries the adapters the fork API needs.
type Deps struct {
	// Forks is the external-contribution command (the forks application
	// service). REQUIRED: without it the route fails closed (503) — a fork
	// is a write into the caller's own space and one that no layer
	// authorized must never happen.
	Forks ForkService
	// Projects is the project read the request body's visibility condition
	// runs (handleFork). It must be the SAME reader every other project
	// read goes through, so "is this project public to this caller" has one
	// answer on this surface and everywhere else. Optional only in the
	// narrow sense that a request asking for a private fork never needs it:
	// a request asking for a public one without it is refused (503), not
	// granted.
	Projects ProjectReader
	// Missing lists the environment keys provisioning needs but that are
	// unset. Non-empty means repository provisioning is DISABLED: the fork
	// route must fail closed (503) naming the missing keys, exactly like the
	// git-token and files surfaces, because a fork without a provisioned
	// repository would write a project row and then panic on a nil adapter.
	Missing []string
}

// New wires the handler.
func New(deps Deps) *API {
	return &API{handlers: &handlers{forks: deps.Forks, projects: deps.Projects, missing: deps.Missing}}
}

// API is the mounted fork surface.
type API struct {
	handlers *handlers
}

// Register mounts the fork route on the /api/v1 mux (no guard — main.go
// wraps the whole subtree in authhttp.API.Guard, so the route is session +
// CSRF protected by construction, which is what the contract's parameters
// promise).
//
// POST /api/v1/projects/{projectId}/forks is the only route here. It
// coexists with the projects surface's subtree (cmd/api/projectshttp
// registers /api/v1/projects/{projectId} for GET and PATCH, which matches
// other segments not at all) because ServeMux matches the most specific
// pattern: the literal "forks" segment is this route and nothing else
// claims it.
func (a *API) Register(v1 *http.ServeMux) {
	v1.HandleFunc("POST /api/v1/projects/{projectId}/forks", a.handlers.handleFork)
}
