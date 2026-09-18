// Package inboxhttp is the research inbox HTTP surface (T1003): read the
// inbox, and mark what was read as read.
//
// Three routes, all of them the caller's own rows:
//
//	GET  /api/v1/inbox            one page of aggregated entries
//	POST /api/v1/inbox/read       mark the entries the caller read
//	POST /api/v1/inbox/read-all   mark what the inbox is serving read
//
// Every route is session + CSRF guarded by the /api/v1 subtree guard
// composed in cmd/api/main.go. A read has no anonymous half — an inbox is
// one person's — so a missing session answers 401 here.
package inboxhttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/inbox"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
)

// handlers owns the inbox routes.
type handlers struct {
	svc *inbox.Service
}

// inboxEntryPayload is the client-visible entry: which target the
// notifications are about, what kind of event they are, the window they
// were aggregated into, the counters, and the link to the subject.
//
// The entry's identity is the tuple (target_type, target_id, event_type,
// window_start) — deliberately spelled out rather than hidden behind an
// opaque id, because it is what the row IS. latest_delivery_id is the
// anchor a mark-read call names: it is the newest delivery the caller was
// shown, so marking it read marks what was displayed and not a delivery
// that arrived afterwards.
type inboxEntryPayload struct {
	TargetType       string    `json:"target_type"`
	TargetID         string    `json:"target_id"`
	TargetLabel      string    `json:"target_label"`
	EventType        string    `json:"event_type"`
	WindowStart      time.Time `json:"window_start"`
	Count            int       `json:"count"`
	Unread           int       `json:"unread"`
	Read             bool      `json:"read"`
	FirstAt          time.Time `json:"first_at"`
	LastAt           time.Time `json:"last_at"`
	LatestEventID    string    `json:"latest_event_id"`
	LatestDeliveryID string    `json:"latest_delivery_id"`
	URL              string    `json:"url"`
}

func entryPayload(e inbox.Entry) inboxEntryPayload {
	return inboxEntryPayload{
		TargetType:       e.TargetType,
		TargetID:         e.TargetID,
		TargetLabel:      e.TargetLabel,
		EventType:        e.EventType,
		WindowStart:      e.WindowStart,
		Count:            e.Count,
		Unread:           e.Unread,
		Read:             e.Read(),
		FirstAt:          e.FirstAt,
		LastAt:           e.LastAt,
		LatestEventID:    e.LatestEventID,
		LatestDeliveryID: e.LatestDeliveryID,
		URL:              e.URL,
	}
}

// handleList: GET /api/v1/inbox — one page of the caller's inbox.
//
//	?filter=unread|all   default unread
//	?limit=N             default 50, capped at 200
//
// The response carries the badge (unread_count, the number of entries with
// anything unread — over the whole inbox, not over this page) and echoes
// the view that was served.
func (h *handlers) handleList(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			authhttp.WriteError(w, r, http.StatusBadRequest, inbox.CodeValidationFailed,
				"limit must be an integer")
			return
		}
		limit = n
	}
	view, err := h.svc.List(r.Context(), actor, r.URL.Query().Get("filter"), limit)
	if err != nil {
		inboxError(w, r, err)
		return
	}
	entries := make([]inboxEntryPayload, 0, len(view.Entries))
	for _, e := range view.Entries {
		entries = append(entries, entryPayload(e))
	}
	authhttp.WriteJSON(w, http.StatusOK, map[string]any{
		"entries":      entries,
		"unread_count": view.UnreadEntries,
		"filter":       view.Filter,
	})
}

// markReadRequest is the mark-read body: the entries the caller read, each
// named by its newest delivery id (inboxEntryPayload.latest_delivery_id).
type markReadRequest struct {
	Deliveries []string `json:"deliveries"`
}

// handleMarkRead: POST /api/v1/inbox/read — mark the entries the caller
// read.
func (h *handlers) handleMarkRead(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var req markReadRequest
	if !decodeBody(w, r, &req) {
		return
	}
	n, err := h.svc.MarkRead(r.Context(), actor, req.Deliveries)
	if err != nil {
		inboxError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, map[string]any{"read": n})
}

// handleMarkAllRead: POST /api/v1/inbox/read-all — mark what the inbox is
// serving the caller read. The scope is the service's and the store's (the
// audience rule), not this layer's: the handler only reports how many
// deliveries were marked.
func (h *handlers) handleMarkAllRead(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	n, err := h.svc.MarkAllRead(r.Context(), actor)
	if err != nil {
		inboxError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, map[string]any{"read": n})
}

// decodeBody parses a JSON body (bounded) and reports success — a
// malformed body is answered in place.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if err := dec.Decode(dst); err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, inbox.CodeValidationFailed,
			"request body must be valid JSON")
		return false
	}
	return true
}

// principal resolves the authenticated actor. The inbox has no anonymous
// read — the deliveries are one person's — so the 401 is written here.
func principal(w http.ResponseWriter, r *http.Request) (domain.User, bool) {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		authhttp.WriteError(w, r, http.StatusUnauthorized, authn.CodeUnauthenticated,
			"authentication required")
		return domain.User{}, false
	}
	return p.User, true
}

// inboxError maps service sentinels to wire codes (the orgs shape).
func inboxError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, inbox.ErrNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, inbox.CodeNotFound,
			"inbox entry not found")
	case errors.Is(err, inbox.ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, inbox.CodeValidationFailed, err.Error())
	case errors.Is(err, inbox.ErrStore):
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, inbox.CodeServiceUnavailable,
			"inbox data is temporarily unavailable")
	default:
		observability.LoggerFromContext(r.Context()).Error("inbox handler: unexpected error", "error", err)
		authhttp.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR",
			"an internal error occurred")
	}
}
