package explorehttp

import (
	"net/http"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/explore"
	"github.com/lichman0405/post/internal/observability"
)

// Wire codes (docs/45). EXPLORE_UNAVAILABLE is this surface's only failure:
// the index is one aggregate read, so there is no per-section error a client
// could act on differently.
const (
	CodeExploreUnavailable = "EXPLORE_UNAVAILABLE"
)

// handlers owns the Explore routes.
type handlers struct {
	svc *explore.Service
}

// handleIndex serves GET /api/v1/explore — the aggregated public index.
//
// It takes no query parameters and reads no principal. The payload is the
// same for every caller (see doc.go); a caller who happens to be signed in
// is not sent a different index, because there is no second index to send:
// the private half of a signed-in caller's world is the signed-in surfaces'
// business (/projects, /notifications), not the open network's directory.
//
// A failure answers 503 and nothing else: no partial index, because a
// section silently missing would read as "there is nothing there" (see
// explore.Service.Index).
func (h *handlers) handleIndex(w http.ResponseWriter, r *http.Request) {
	index, err := h.svc.Index(r.Context())
	if err != nil {
		observability.LoggerFromContext(r.Context()).Error("explore: index read failed", "error", err)
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, CodeExploreUnavailable,
			"the network index is temporarily unavailable")
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, index)
}
