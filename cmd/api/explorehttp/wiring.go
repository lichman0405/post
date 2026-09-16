package explorehttp

import (
	"net/http"

	"github.com/lichman0405/post/internal/application/explore"
)

// Deps carries the reader the surface needs.
type Deps struct {
	// Reader is the six-read port internal/application/explore defines. The
	// production value is Sources{} (adapters.go + store.go) — the three
	// reuse adapters plus the PostgreSQL store for the sections that had no
	// read before T0802.
	Reader explore.Reader
}

// New wires the handlers.
func New(deps Deps) *API {
	return &API{handlers: &handlers{svc: explore.NewService(deps.Reader)}}
}

// API is the mounted Explore surface.
type API struct {
	handlers *handlers
}

// Register mounts the route on the shared /api/v1 mux.
//
// One route, the full path: /api/v1/explore names the surface rather than
// any entity, and the answer is the whole index (six sections). No method
// but GET has a route — there is nothing on this surface to write.
func (a *API) Register(v1 *http.ServeMux) {
	v1.HandleFunc("GET /api/v1/explore", a.handlers.handleIndex)
}
