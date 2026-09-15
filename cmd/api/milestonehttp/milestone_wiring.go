package milestonehttp

import (
	"net/http"
)

// The milestone API wiring: the milestone command and the project read
// gate go in, guarded routes come out (the guard itself is owned by
// authhttp and composed in cmd/api/main.go). cmd/api builds the
// production graph; the integration suite builds the same graph over the
// same real PostgreSQL.

// Deps carries the command and the visibility gate the milestone API
// needs.
type Deps struct {
	// Command is the wired milestone command (CommandPort; the production
	// value is *milestones.Command).
	Command CommandPort
	// Projects is the project read gate: a milestone is exactly as
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

// API is the mounted milestone surface.
type API struct {
	handlers *handlers
}

// Register mounts the milestone routes directly on the v1 mux. The
// full-path patterns are more specific than the /api/v1/projects/
// subtree, so they coexist with it (same technique as releasehttp).
// There is deliberately no PUT/PATCH/DELETE registration: V1 has no
// correction surface — a recorded milestone is a fact on the timeline
// (the task's basics; a correction surface is a follow-up decision).
func (a *API) Register(v1 *http.ServeMux) {
	h := a.handlers
	v1.HandleFunc("GET /api/v1/projects/{projectId}/milestones", h.handleListMilestones)
	v1.HandleFunc("POST /api/v1/projects/{projectId}/milestones", h.handleCreateMilestone)
	v1.HandleFunc("GET /api/v1/projects/{projectId}/milestones/{milestoneId}", h.handleGetMilestone)
}
