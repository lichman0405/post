package feedshttp

import (
	"context"
	"net/http"

	"github.com/lichman0405/post/internal/application/feeds"
)

// Service is the feed use case as this transport uses it: one call that
// answers one target's document in one format, error included. The
// production value is *feeds.Service over *persistence.FeedStore.
//
// The port is deliberately one method wide. The transport renders; it never
// asks for a model it does not write out, so a second method here would be a
// seam nothing used.
type Service interface {
	Document(ctx context.Context, target feeds.Target, format feeds.Format) ([]byte, error)
}

// Deps carries what the surface needs.
type Deps struct {
	// Service is the feed use case. A nil Service means the surface is NOT
	// WIRED — which happens when cmd/api could not build it, i.e. when the
	// configured public origin is not a usable base for absolute links. The
	// routes are registered anyway and answer 503 naming the deployment
	// problem, the way the git-token and files surfaces answer when their
	// configuration is missing: a feed that cannot be built is not a feed
	// that does not exist, and answering 404 would say the latter.
	Service Service
}

// New wires the handlers.
func New(deps Deps) *API {
	return &API{handlers: &handlers{svc: deps.Service}}
}

// API is the mounted feed surface.
type API struct {
	handlers *handlers
}

// Register mounts the three feed routes on the shared /api/v1 mux.
//
// The patterns are full paths (not a subtree), so they coexist with the
// entity routes of the same names — /api/v1/assets/{assetId} and
// /api/v1/feeds/assets/{assetId} are distinct ServeMux patterns and neither
// shadows the other. Only GET (and HEAD, which ServeMux folds into GET) has a
// route: there is nothing on this surface to write, and a client that POSTs
// to a feed URL is answered 405 by the mux rather than reaching a handler.
func (a *API) Register(v1 *http.ServeMux) {
	v1.HandleFunc("GET /api/v1/feeds/projects/{projectId}", a.handlers.handleProjectFeed)
	v1.HandleFunc("GET /api/v1/feeds/assets/{assetId}", a.handlers.handleAssetFeed)
	v1.HandleFunc("GET /api/v1/feeds/knowledge/{objectId}", a.handlers.handleKnowledgeFeed)
}
