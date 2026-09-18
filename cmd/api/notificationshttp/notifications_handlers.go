// Package notificationshttp is the email-notification settings surface
// (T1005): read and change the account's digest frequency. Both routes are
// session + CSRF guarded by the /api/v1 subtree guard composed in
// cmd/api/main.go, and both act on the CALLER's own row — there is no
// path parameter naming a user, so one account can never read or set
// another's cadence.
package notificationshttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/notifications"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/observability"
)

// handlers owns the notification-settings routes.
type handlers struct {
	svc *notifications.Service
}

// preferencesPayload is the client-visible shape of the setting. body carries
// no address and no user id: the caller is the subject of the response, and
// echoing the address back would put it in one more place for no reader.
type preferencesPayload struct {
	Cadence string `json:"cadence"`
	// Options is the closed vocabulary, so a client renders the choices
	// without hardcoding them (the same list the command accepts).
	Options []string `json:"options"`
	// LastDigestAt is when this account's last digest was handed to the
	// transport; null when none has been sent (or email delivery is off).
	LastDigestAt *time.Time `json:"last_digest_at"`
	// Stored reports whether the account has ever changed the setting.
	// False means the account is on the platform default, which cadence
	// states — "unset" is not a value, it is a state.
	Stored    bool      `json:"stored"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CadenceOptions is the selectable list, in the order a client should render
// it: calmest last is not the order — the default comes first, and the
// noisier choice is explicit.
var CadenceOptions = []string{events.CadenceDaily, events.CadenceImmediate, events.CadenceWeekly}

func payloadFromPrefs(p events.NotificationPreferences) preferencesPayload {
	return preferencesPayload{
		Cadence:      p.EffectiveCadence(),
		Options:      CadenceOptions,
		LastDigestAt: p.LastDigestAt,
		Stored:       p.Stored,
		UpdatedAt:    p.UpdatedAt,
	}
}

// principal resolves the authenticated actor; reads require a session too
// (the setting is account-private), so the 401 is written here.
func principal(w http.ResponseWriter, r *http.Request) (domain.User, bool) {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		authhttp.WriteError(w, r, http.StatusUnauthorized, authn.CodeUnauthenticated,
			"authentication required")
		return domain.User{}, false
	}
	return p.User, true
}

// notificationsError maps service sentinels to wire codes (the
// subscriptions shape).
func notificationsError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, notifications.ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, notifications.CodeValidationFailed, err.Error())
	case errors.Is(err, notifications.ErrStore):
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, notifications.CodeServiceUnavailable,
			"notification settings are temporarily unavailable")
	default:
		observability.LoggerFromContext(r.Context()).Error("notifications handler: unexpected error", "error", err)
		authhttp.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR",
			"an internal error occurred")
	}
}

// handleGetPreferences: GET /api/v1/notifications/preferences — the
// caller's cadence.
func (h *handlers) handleGetPreferences(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	prefs, err := h.svc.Preferences(r.Context(), actor)
	if err != nil {
		notificationsError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, map[string]any{"preferences": payloadFromPrefs(prefs)})
}

// setCadenceRequest is the PUT body. cadence is REQUIRED and must be one of
// the three; an absent field is refused rather than defaulted (see
// notifications.Service.SetCadence).
type setCadenceRequest struct {
	Cadence string `json:"cadence"`
}

// handleSetCadence: PUT /api/v1/notifications/preferences — replace the
// caller's cadence. PUT, not PATCH: the body carries the whole setting, and
// the values are a closed vocabulary a client picks from.
func (h *handlers) handleSetCadence(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var req setCadenceRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if err := dec.Decode(&req); err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, notifications.CodeValidationFailed,
			"request body must be valid JSON")
		return
	}
	prefs, err := h.svc.SetCadence(r.Context(), actor, req.Cadence)
	if err != nil {
		notificationsError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, map[string]any{"preferences": payloadFromPrefs(prefs)})
}
