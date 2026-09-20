package discussionhttp

import (
	"net/http"
)

// The discussion API wiring: the discussion command goes in, guarded routes
// come out (the guard itself is owned by authhttp and composed in
// cmd/api/main.go). cmd/api builds the production graph; the integration
// suite builds the same graph over the same real PostgreSQL.

// Deps carries the command the surface calls. The production value is
// *discussions.Command.
type Deps struct {
	// Command is the wired discussion command (CommandPort).
	Command CommandPort
}

// New wires the handlers.
func New(deps Deps) *API {
	return &API{handlers: &handlers{cmd: deps.Command}}
}

// API is the mounted discussion surface.
type API struct {
	handlers *handlers
}

// Register mounts the discussion routes on the guarded v1 mux.
//
// Two route-shape notes, since Go's ServeMux resolves by specificity:
//
//   - `.../discussions/promotions` and `.../discussions/promotions/{id}`
//     are literal segments, so they never collide with
//     `.../discussions/{threadId}`, which matches exactly one segment.
//   - The target filter of the list route is a QUERY pair
//     (target_type, target_id), not a path, because a thread's target is
//     polymorphic: three surfaces, three addressing schemes, one route
//     family (the reason is in internal/domain/discussion.go).
//
// There is deliberately no route that edits or deletes a thread, and none
// that edits a comment: a conversation is append-only and a withdrawal is a
// tombstone (DELETE), never an erasure.
func (a *API) Register(v1 *http.ServeMux) {
	h := a.handlers
	v1.HandleFunc("POST /api/v1/projects/{projectId}/discussions", h.handleOpenThread)
	v1.HandleFunc("GET /api/v1/projects/{projectId}/discussions", h.handleListThreads)
	v1.HandleFunc("GET /api/v1/projects/{projectId}/discussions/{threadId}", h.handleGetThread)
	v1.HandleFunc("POST /api/v1/projects/{projectId}/discussions/{threadId}/comments", h.handleAddComment)
	v1.HandleFunc("DELETE /api/v1/projects/{projectId}/discussions/{threadId}/comments/{commentId}", h.handleDeleteComment)
	v1.HandleFunc("POST /api/v1/projects/{projectId}/discussions/{threadId}/comments/{commentId}/promotions", h.handlePromote)
	v1.HandleFunc("GET /api/v1/projects/{projectId}/discussions/promotions", h.handleListPromotions)
	v1.HandleFunc("GET /api/v1/projects/{projectId}/discussions/promotions/{promotionId}", h.handleGetPromotion)
}
