// Package subscriptionshttp is the follow/watch HTTP surface (T1002):
// create, read, change and end a subscription. Every route is session +
// CSRF guarded by the /api/v1 subtree guard composed in cmd/api/main.go,
// and every handler is owner-scoped — a subscription id that is not the
// caller's answers the same 404 an unknown id does.
package subscriptionshttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/subscriptions"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/observability"
)

// handlers owns the subscription routes.
type handlers struct {
	svc *subscriptions.Service
}

// subscriptionPayload is the client-visible subscription shape: the target
// (type + id), what reaches the subscriber, and over which channels.
// It carries no field identifying anyone but the owner — the row is the
// owner's — and no deleted_at: an ended subscription is not readable, so
// rendering one would only ever print null.
type subscriptionPayload struct {
	ID           string    `json:"id"`
	TargetType   string    `json:"target_type"`
	TargetID     string    `json:"target_id"`
	EventFilters []string  `json:"event_filters"`
	Channels     []string  `json:"channels"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func subscriptionPayloadFromDomain(s events.Subscription) subscriptionPayload {
	return subscriptionPayload{
		ID:           s.ID,
		TargetType:   s.TargetType,
		TargetID:     s.TargetID,
		EventFilters: emptyIfNil(s.EventFilters),
		Channels:     emptyIfNil(s.Channels),
		CreatedAt:    s.CreatedAt,
		UpdatedAt:    s.UpdatedAt,
	}
}

// emptyIfNil renders a nil list as [] rather than null: "no filters" means
// every event type, and a client should not have to tell null from [].
func emptyIfNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// decodeBody parses a JSON body (bounded) and reports success — a
// malformed body is answered in place.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if err := dec.Decode(dst); err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, subscriptions.CodeValidationFailed,
			"request body must be valid JSON")
		return false
	}
	return true
}

// principal resolves the authenticated actor; reads require a session too
// (subscriptions are owner-private), so the 401 is written here.
func principal(w http.ResponseWriter, r *http.Request) (domain.User, bool) {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		authhttp.WriteError(w, r, http.StatusUnauthorized, authn.CodeUnauthenticated,
			"authentication required")
		return domain.User{}, false
	}
	return p.User, true
}

// subscriptionError maps service sentinels to wire codes (the orgs shape).
func subscriptionError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, subscriptions.ErrNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, subscriptions.CodeNotFound,
			"subscription not found")
	case errors.Is(err, subscriptions.ErrExists):
		authhttp.WriteError(w, r, http.StatusConflict, subscriptions.CodeExists,
			"already subscribed to this target")
	case errors.Is(err, subscriptions.ErrLimit):
		authhttp.WriteError(w, r, http.StatusConflict, subscriptions.CodeLimit,
			"subscription limit reached")
	case errors.Is(err, subscriptions.ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, subscriptions.CodeValidationFailed, err.Error())
	case errors.Is(err, subscriptions.ErrStore):
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, subscriptions.CodeServiceUnavailable,
			"subscription data is temporarily unavailable")
	default:
		observability.LoggerFromContext(r.Context()).Error("subscriptions handler: unexpected error", "error", err)
		authhttp.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR",
			"an internal error occurred")
	}
}

// createSubscriptionRequest is the subscribe body: the target (type + id),
// the event types that should reach the subscriber (empty = all), and the
// channels. channels is REQUIRED — a subscription with no channel would
// record an intent that can never be delivered, and defaulting it would be
// this layer inventing a product rule.
type createSubscriptionRequest struct {
	TargetType   string   `json:"target_type"`
	TargetID     string   `json:"target_id"`
	EventFilters []string `json:"event_filters"`
	Channels     []string `json:"channels"`
}

// handleCreate: POST /api/v1/subscriptions — follow a target.
func (h *handlers) handleCreate(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var req createSubscriptionRequest
	if !decodeBody(w, r, &req) {
		return
	}
	sub, err := h.svc.Subscribe(r.Context(), actor,
		events.Target{Type: req.TargetType, ID: req.TargetID}, req.EventFilters, req.Channels)
	if err != nil {
		subscriptionError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusCreated, map[string]any{
		"subscription": subscriptionPayloadFromDomain(sub),
	})
}

// handleList: GET /api/v1/subscriptions[?target_type=&target_id=] — the
// actor's live subscriptions, optionally narrowed to one target (the
// watch-state read).
func (h *handlers) handleList(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	list, err := h.svc.List(r.Context(), actor, q.Get("target_type"), q.Get("target_id"))
	if err != nil {
		subscriptionError(w, r, err)
		return
	}
	out := make([]subscriptionPayload, 0, len(list))
	for _, s := range list {
		out = append(out, subscriptionPayloadFromDomain(s))
	}
	authhttp.WriteJSON(w, http.StatusOK, map[string]any{"subscriptions": out})
}

// handleGet: GET /api/v1/subscriptions/{subscriptionId} — one owned
// subscription.
func (h *handlers) handleGet(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	sub, err := h.svc.Get(r.Context(), actor, r.PathValue("subscriptionId"))
	if err != nil {
		subscriptionError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, subscriptionPayloadFromDomain(sub))
}

// updateSubscriptionRequest is the PATCH body: filters and channels are
// full replacements. The target is absent by design — following something
// else is a new subscription, not an edit (subscriptions.Service.Update).
type updateSubscriptionRequest struct {
	EventFilters []string `json:"event_filters"`
	Channels     []string `json:"channels"`
}

// handleUpdate: PATCH /api/v1/subscriptions/{subscriptionId} — change what
// the subscription lets through and over which channels.
func (h *handlers) handleUpdate(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var req updateSubscriptionRequest
	if !decodeBody(w, r, &req) {
		return
	}
	sub, err := h.svc.Update(r.Context(), actor, r.PathValue("subscriptionId"), req.EventFilters, req.Channels)
	if err != nil {
		subscriptionError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, subscriptionPayloadFromDomain(sub))
}

// handleUnsubscribe: DELETE /api/v1/subscriptions/{subscriptionId} — end
// the follow. Deliveries still in flight are cancelled with it; rows
// already delivered stay (nothing disappears — CLAUDE.md §9).
func (h *handlers) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	if err := h.svc.Unsubscribe(r.Context(), actor, r.PathValue("subscriptionId")); err != nil {
		subscriptionError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusNoContent, map[string]any{})
}
