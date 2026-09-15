package webhookshttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/webhooks"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/observability"
)

// The webhook endpoint HTTP surface (T1006). Every handler: resolve the
// principal (the guard put it there), parse the request, call the
// Service, render the payload or the standard error envelope. The signing
// secret is rendered EXACTLY twice ever: the create and rotate responses.

// handlers owns the webhook routes.
type handlers struct {
	svc *webhooks.Service
}

// endpointPayload is the client-visible endpoint shape. No secret field
// exists on it by construction — the secret travels in the dedicated
// create/rotate response field only.
type endpointPayload struct {
	ID                  string     `json:"id"`
	URL                 string     `json:"url"`
	EventFilters        []string   `json:"event_filters"`
	Enabled             bool       `json:"enabled"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	DisabledAt          *time.Time `json:"disabled_at"`
	CreatedAt           time.Time  `json:"created_at"`
}

func endpointPayloadFromDomain(e events.WebhookEndpoint) endpointPayload {
	return endpointPayload{
		ID:                  e.ID,
		URL:                 e.URL,
		EventFilters:        e.EventFilters,
		Enabled:             e.Enabled,
		ConsecutiveFailures: e.ConsecutiveFailures,
		DisabledAt:          e.DisabledAt,
		CreatedAt:           e.CreatedAt,
	}
}

// deliveryPayload is the client-visible delivery-log row.
type deliveryPayload struct {
	ID            string     `json:"id"`
	EndpointID    string     `json:"endpoint_id"`
	EventID       string     `json:"event_id"`
	EventType     string     `json:"event_type"`
	EndpointURL   string     `json:"endpoint_url"`
	Status        string     `json:"status"`
	ResponseCode  *int       `json:"response_code"`
	Attempts      int        `json:"attempts"`
	LastAttemptAt *time.Time `json:"last_attempt_at"`
	NextRetryAt   *time.Time `json:"next_retry_at"`
	LastError     *string    `json:"last_error"`
	DeliveredAt   *time.Time `json:"delivered_at"`
	CreatedAt     time.Time  `json:"created_at"`
}

func deliveryPayloadFromDomain(d events.Delivery) deliveryPayload {
	return deliveryPayload{
		ID:            d.ID,
		EndpointID:    d.EndpointID,
		EventID:       d.EventID,
		EventType:     d.EventType,
		EndpointURL:   d.EndpointURL,
		Status:        d.Status,
		ResponseCode:  d.ResponseCode,
		Attempts:      d.Attempts,
		LastAttemptAt: d.LastAttemptAt,
		NextRetryAt:   d.NextRetryAt,
		LastError:     d.LastError,
		DeliveredAt:   d.DeliveredAt,
		CreatedAt:     d.CreatedAt,
	}
}

// decodeBody parses a JSON body (bounded) and reports success — a
// malformed body is answered in place.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if err := dec.Decode(dst); err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, webhooks.CodeValidationFailed,
			"request body must be valid JSON")
		return false
	}
	return true
}

// principal resolves the authenticated actor; reads require a session too
// (endpoints are owner-private), so the 401 is written here.
func principal(w http.ResponseWriter, r *http.Request) (domain.User, bool) {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		authhttp.WriteError(w, r, http.StatusUnauthorized, authn.CodeUnauthenticated,
			"authentication required")
		return domain.User{}, false
	}
	return p.User, true
}

// webhookError maps service sentinels to wire codes (the orgs shape).
func webhookError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, webhooks.ErrNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, webhooks.CodeEndpointNotFound,
			"webhook endpoint not found")
	case errors.Is(err, webhooks.ErrLimit):
		authhttp.WriteError(w, r, http.StatusConflict, webhooks.CodeEndpointLimit,
			"webhook endpoint limit reached")
	case errors.Is(err, webhooks.ErrNotRedeliverable):
		authhttp.WriteError(w, r, http.StatusConflict, webhooks.CodeNotRedeliverable,
			"delivery is not redeliverable")
	case errors.Is(err, webhooks.ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, webhooks.CodeValidationFailed, err.Error())
	case errors.Is(err, webhooks.ErrStore):
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, webhooks.CodeServiceUnavailable,
			"webhook data is temporarily unavailable")
	default:
		observability.LoggerFromContext(r.Context()).Error("webhooks handler: unexpected error", "error", err)
		authhttp.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR",
			"an internal error occurred")
	}
}

type createWebhookRequest struct {
	URL          string   `json:"url"`
	EventFilters []string `json:"event_filters"`
}

// handleCreate: POST /api/v1/webhooks — register an endpoint. The
// response carries the generated secret; it is never shown again.
func (h *handlers) handleCreate(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var req createWebhookRequest
	if !decodeBody(w, r, &req) {
		return
	}
	created, err := h.svc.Create(r.Context(), actor, req.URL, req.EventFilters)
	if err != nil {
		webhookError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusCreated, map[string]any{
		"webhook": endpointPayloadFromDomain(created.WebhookEndpoint),
		// The one-time secret. Logging never sees it: WriteJSON does not
		// log bodies.
		"secret": created.Secret,
	})
}

// handleList: GET /api/v1/webhooks — the actor's endpoints.
func (h *handlers) handleList(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	list, err := h.svc.List(r.Context(), actor)
	if err != nil {
		webhookError(w, r, err)
		return
	}
	out := make([]endpointPayload, 0, len(list))
	for _, e := range list {
		out = append(out, endpointPayloadFromDomain(e))
	}
	authhttp.WriteJSON(w, http.StatusOK, map[string]any{"webhooks": out})
}

// handleGet: GET /api/v1/webhooks/{webhookId} — one owned endpoint.
func (h *handlers) handleGet(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	e, err := h.svc.Get(r.Context(), actor, r.PathValue("webhookId"))
	if err != nil {
		webhookError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, endpointPayloadFromDomain(e))
}

type updateWebhookRequest struct {
	// Pointer fields: PATCH partial-update semantics — an absent field
	// means "unchanged". enabled:true re-enables a policy-disabled
	// endpoint and resets its failure streak.
	URL          *string  `json:"url"`
	EventFilters []string `json:"event_filters"`
	Enabled      *bool    `json:"enabled"`
}

// handleUpdate: PATCH /api/v1/webhooks/{webhookId} — url, filters,
// enabled.
func (h *handlers) handleUpdate(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var req updateWebhookRequest
	if !decodeBody(w, r, &req) {
		return
	}
	e, err := h.svc.Update(r.Context(), actor, r.PathValue("webhookId"), req.URL, req.EventFilters, req.Enabled)
	if err != nil {
		webhookError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, endpointPayloadFromDomain(e))
}

// handleRotateSecret: POST /api/v1/webhooks/{webhookId}/secret — replace
// the signing secret. The response carries the new secret; it is never
// shown again.
func (h *handlers) handleRotateSecret(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	rotated, err := h.svc.RegenerateSecret(r.Context(), actor, r.PathValue("webhookId"))
	if err != nil {
		webhookError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, map[string]any{
		"webhook": endpointPayloadFromDomain(rotated.WebhookEndpoint),
		"secret":  rotated.Secret,
	})
}

// handleDelete: DELETE /api/v1/webhooks/{webhookId} — remove the
// endpoint; its pending deliveries are cancelled with it, finished
// delivery-log rows stay.
func (h *handlers) handleDelete(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), actor, r.PathValue("webhookId")); err != nil {
		webhookError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusNoContent, map[string]any{})
}

// handleListDeliveries: GET /api/v1/webhooks/{webhookId}/deliveries — the
// delivery log, newest first. before carries "<created_at RFC3339>,<id>"
// for keyset pagination.
func (h *handlers) handleListDeliveries(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var beforeTS *time.Time
	beforeID := ""
	if raw := r.URL.Query().Get("before"); raw != "" {
		tsPart, idPart, found := strings.Cut(raw, ",")
		if !found {
			authhttp.WriteError(w, r, http.StatusBadRequest, webhooks.CodeValidationFailed,
				`before must be "<RFC3339 timestamp>,<id>"`)
			return
		}
		ts, err := time.Parse(time.RFC3339, tsPart)
		if err != nil {
			authhttp.WriteError(w, r, http.StatusBadRequest, webhooks.CodeValidationFailed,
				`before timestamp must be RFC3339`)
			return
		}
		beforeTS, beforeID = &ts, idPart
	}
	list, err := h.svc.ListDeliveries(r.Context(), actor, r.PathValue("webhookId"), beforeTS, beforeID)
	if err != nil {
		webhookError(w, r, err)
		return
	}
	out := make([]deliveryPayload, 0, len(list))
	for _, d := range list {
		out = append(out, deliveryPayloadFromDomain(d))
	}
	authhttp.WriteJSON(w, http.StatusOK, map[string]any{"deliveries": out})
}

// handleRedeliver: POST /api/v1/webhooks/{webhookId}/deliveries/{deliveryId}/retry
// — re-queue one finished delivery. The delivery id stays stable, so an
// idempotent consumer sees the duplicate coming.
func (h *handlers) handleRedeliver(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	err := h.svc.Redeliver(r.Context(), actor, r.PathValue("webhookId"), r.PathValue("deliveryId"))
	if err != nil {
		webhookError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusNoContent, map[string]any{})
}
