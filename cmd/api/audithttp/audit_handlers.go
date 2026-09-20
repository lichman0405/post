package audithttp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/audit"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/orgs"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
)

// The Activity HTTP surface: resolve the principal (reads require a
// session, like the project/org surfaces), translate the query params,
// call the Service, render the entries. No handler here writes — the
// audit log has no update/delete surface.

// handlers owns the Activity routes.
type handlers struct {
	svc *audit.Service
}

// activityEntryPayload is the client-visible Activity row shape. The jsonb
// summaries are embedded raw — they are already canonical JSON.
//
// source (T0607) is always present and is what tells the page which
// registry the row came from: "governance" (audit_log) or "research"
// (research_events). The payload is the research row's event body and
// visibility the visibility it was recorded with; both are absent on a
// governance row, exactly as target_ref and the before/after summaries are
// absent on a research row. Nothing is inferred from which keys are
// missing: the source says which half is meaningful.
type activityEntryPayload struct {
	ID               string          `json:"id"`
	Source           string          `json:"source"`
	ActorID          *string         `json:"actor_id"`
	ActorHandle      *string         `json:"actor_handle"`
	ActorDisplayName *string         `json:"actor_display_name"`
	Via              string          `json:"via"`
	Action           string          `json:"action"`
	TargetRef        *string         `json:"target_ref"`
	ProjectID        *string         `json:"project_id"`
	OrganizationID   *string         `json:"organization_id"`
	CorrelationID    string          `json:"correlation_id"`
	BeforeSummary    json.RawMessage `json:"before_summary,omitempty"`
	AfterSummary     json.RawMessage `json:"after_summary,omitempty"`
	Metadata         json.RawMessage `json:"metadata,omitempty"`
	Payload          json.RawMessage `json:"payload,omitempty"`
	Visibility       string          `json:"visibility,omitempty"`
	OccurredAt       time.Time       `json:"occurred_at"`
}

func activityEntryFromDomain(r domain.AuditRecord) activityEntryPayload {
	return activityEntryPayload{
		ID:               r.ID,
		Source:           string(r.Source),
		ActorID:          r.ActorID,
		ActorHandle:      r.ActorHandle,
		ActorDisplayName: r.ActorDisplayName,
		Via:              r.Via,
		Action:           r.Action,
		TargetRef:        r.TargetRef,
		ProjectID:        r.ProjectID,
		OrganizationID:   r.OrganizationID,
		CorrelationID:    r.CorrelationID,
		BeforeSummary:    json.RawMessage(r.BeforeSummary),
		AfterSummary:     json.RawMessage(r.AfterSummary),
		Metadata:         json.RawMessage(r.Metadata),
		Payload:          json.RawMessage(r.Payload),
		Visibility:       r.Visibility,
		OccurredAt:       r.OccurredAt,
	}
}

// pageParams parses the cursor and limit query params. A missing limit
// means the service default; a malformed one is a validation error.
func pageParams(r *http.Request) (cursor string, limit int, err error) {
	cursor = r.URL.Query().Get("cursor")
	limit = audit.DefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil {
			return "", 0, audit.ErrValidation
		}
	}
	return cursor, limit, nil
}

// sourceParam parses the source filter: absent (or "source=") means the
// page reads both registries, "governance" and "research" narrow it to
// one. Any other value is a validation error — never an empty page, which
// is what silently ignoring an unknown filter would look like (a reader
// who mistyped a value would be told "this project has no such history").
func sourceParam(r *http.Request) (domain.ActivitySource, error) {
	source, err := domain.ParseActivitySource(r.URL.Query().Get("source"))
	if err != nil {
		return "", fmt.Errorf("%w: %v", audit.ErrValidation, err)
	}
	return source, nil
}

// handleProjectActivity: GET /api/v1/projects/{projectId}/activity — the
// project's Activity, newest first, member-only (the same visibility as
// the project itself): the governance rows (audit_log) and the research
// events, or one of the two when ?source=governance|research says so. Any
// other verb answers 405: the log is read-only.
func (h *handlers) handleProjectActivity(w http.ResponseWriter, r *http.Request) {
	if !readOnly(w, r) {
		return
	}
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	cursor, limit, err := pageParams(r)
	if err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, audit.CodeValidationFailed,
			"cursor and limit must be valid")
		return
	}
	source, err := sourceParam(r)
	if err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, audit.CodeValidationFailed,
			"source must be governance or research (or absent for both)")
		return
	}
	entries, next, err := h.svc.ProjectActivity(r.Context(), actor, r.PathValue("projectId"), source, cursor, limit)
	if err != nil {
		h.activityError(w, r, err)
		return
	}
	writeActivity(w, entries, next)
}

// handleOrgActivity: GET /api/v1/organizations/{orgId}/activity — the
// organization's governance log, newest first, member-only. Read-only like
// the project feed. ?source=governance (or no source) is the whole feed;
// ?source=research is refused rather than answered empty, because research
// events are project-scoped and an empty page would report that gap as a
// fact about the organization (internal/application/audit.Service).
func (h *handlers) handleOrgActivity(w http.ResponseWriter, r *http.Request) {
	if !readOnly(w, r) {
		return
	}
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	cursor, limit, err := pageParams(r)
	if err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, audit.CodeValidationFailed,
			"cursor and limit must be valid")
		return
	}
	source, err := sourceParam(r)
	if err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, audit.CodeValidationFailed,
			"source must be governance (or absent)")
		return
	}
	entries, next, err := h.svc.OrgActivity(r.Context(), actor, r.PathValue("orgId"), source, cursor, limit)
	if err != nil {
		h.activityError(w, r, err)
		return
	}
	writeActivity(w, entries, next)
}

// readOnly admits GET and HEAD — HEAD mirrors GET (same status and
// headers, no body; the server discards the write) — and refuses every
// other verb with 405 + Allow. The acceptance "no update/delete audit
// endpoint" is answered explicitly at the surface instead of falling
// through to a 404 (the guard has already required a session + CSRF for
// writes, so the 405 names the read-only rule, not the guard's).
func readOnly(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", http.MethodGet+", "+http.MethodHead)
	authhttp.WriteError(w, r, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED",
		"the audit log is read-only")
	return false
}

// writeActivity renders one page: the entries plus the next-page cursor
// (null when the page ends the log).
func writeActivity(w http.ResponseWriter, entries []domain.AuditRecord, next string) {
	out := make([]activityEntryPayload, 0, len(entries))
	for _, e := range entries {
		out = append(out, activityEntryFromDomain(e))
	}
	payload := map[string]any{"entries": out}
	if next != "" {
		payload["next_cursor"] = next
	} else {
		payload["next_cursor"] = nil
	}
	authhttp.WriteJSON(w, http.StatusOK, payload)
}

// principal resolves the authenticated actor; reads require a session
// (activity is member-only, like the project/org reads), so the 401 is
// written here.
func principal(w http.ResponseWriter, r *http.Request) (domain.User, bool) {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		authhttp.WriteError(w, r, http.StatusUnauthorized, authn.CodeUnauthenticated,
			"authentication required")
		return domain.User{}, false
	}
	return p.User, true
}

// activityError maps Service errors to the wire. Denied reads reuse the
// owning surface's sentinels and therefore its stable codes and envelopes
// (PROJECT_NOT_FOUND / ORG_NOT_FOUND — existence hiding everywhere).
func (h *handlers) activityError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, projects.ErrProjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, projects.CodeProjectNotFound,
			"project not found")
	case errors.Is(err, orgs.ErrOrgNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, orgs.CodeOrgNotFound,
			"organization not found")
	case errors.Is(err, audit.ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, audit.CodeValidationFailed, err.Error())
	case errors.Is(err, projects.ErrStore), errors.Is(err, orgs.ErrStore), errors.Is(err, audit.ErrStore):
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, audit.CodeServiceUnavailable,
			"activity data is temporarily unavailable")
	default:
		observability.LoggerFromContext(r.Context()).Error("activity handler: unexpected error", "error", err)
		authhttp.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR",
			"an internal error occurred")
	}
}
