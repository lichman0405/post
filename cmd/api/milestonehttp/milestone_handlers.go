package milestonehttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/milestones"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
)

// CommandPort is the milestone command slice the transport needs. The
// production implementation is *milestones.Command; the interface exists
// so the handlers are unit-testable against a fake.
type CommandPort interface {
	Create(ctx context.Context, actor domain.User, in milestones.CreateMilestoneParams) (domain.Milestone, error)
	List(ctx context.Context, projectID string) ([]domain.Milestone, error)
	Get(ctx context.Context, projectID, milestoneID string) (domain.Milestone, error)
}

// ProjectVisibilityPort is the read gate every milestone read runs: a
// milestone is exactly as visible as the project (the T0106 read matrix —
// the same resolution every other project read runs). The production
// adapter is *projects.Service (its Get).
type ProjectVisibilityPort interface {
	Get(ctx context.Context, r projects.Reader, projectID string) (domain.Project, error)
}

// handlers owns the milestone routes.
type handlers struct {
	cmd      CommandPort
	projects ProjectVisibilityPort
}

// milestonePayload is the client-visible milestone shape. Label is null
// when the milestone carries no custom label (the kind's display name
// renders then); release_id is null when the milestone documents no
// release (the link is optional, never required).
type milestonePayload struct {
	ID         string    `json:"id"`
	ProjectID  string    `json:"project_id"`
	Kind       string    `json:"kind"`
	Label      *string   `json:"label"`
	OccurredAt time.Time `json:"occurred_at"`
	ReleaseID  *string   `json:"release_id"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
}

func milestonePayloadFromDomain(m domain.Milestone) milestonePayload {
	var label *string
	if m.Label != "" {
		l := m.Label
		label = &l
	}
	return milestonePayload{
		ID:         m.ID,
		ProjectID:  m.ProjectID,
		Kind:       string(m.Kind),
		Label:      label,
		OccurredAt: m.OccurredAt,
		ReleaseID:  m.ReleaseID,
		CreatedBy:  m.CreatedBy,
		CreatedAt:  m.CreatedAt,
	}
}

// createMilestoneRequest is the POST body: the kind, the custom label
// (required for custom, optional otherwise), the occurred_at date (the
// timeline position) and the optional release link.
type createMilestoneRequest struct {
	Kind       string `json:"kind"`
	Label      string `json:"label"`
	OccurredAt string `json:"occurred_at"`
	ReleaseID  string `json:"release_id"`
}

// principal resolves the authenticated actor for the create; a missing
// session is answered in place with 401 (the guard enforces the same
// before routing — this is the handler-level backstop).
func principal(w http.ResponseWriter, r *http.Request) (domain.User, bool) {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		authhttp.WriteError(w, r, http.StatusUnauthorized, authn.CodeUnauthenticated,
			"authentication required")
		return domain.User{}, false
	}
	return p.User, true
}

// reader resolves the caller for the visibility-aware reads: the same
// resolution every other project read runs (anonymous stays anonymous —
// a public project's milestones are public).
func reader(r *http.Request) projects.Reader {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		return projects.Reader{}
	}
	return projects.Reader{UserID: p.User.ID, Authenticated: true}
}

// decodeBody parses the create body (bounded; unknown fields ignored per
// contract) and reports success — a malformed body is answered in place.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := dec.Decode(dst); err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, milestones.CodeMilestoneValidationFailed,
			"request body must be valid JSON")
		return false
	}
	return true
}

// parseOccurredAt parses the timeline date: RFC 3339 with second
// precision (the same shape the API renders timestamps in).
func parseOccurredAt(w http.ResponseWriter, r *http.Request, raw string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, milestones.CodeMilestoneValidationFailed,
			"occurred_at must be an RFC 3339 timestamp")
		return time.Time{}, false
	}
	return t, true
}

// writeMilestoneError maps a command error onto the wire envelope
// (docs/45): one code per failure shape. Dependency failures answer a
// generic 503 naming nothing internal.
func writeMilestoneError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, milestones.ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, milestones.CodeMilestoneValidationFailed, err.Error())
	case errors.Is(err, milestones.ErrForbidden):
		authhttp.WriteError(w, r, http.StatusForbidden, milestones.CodeMilestoneForbidden,
			"the actor may not record milestones here")
	case errors.Is(err, milestones.ErrProjectNotFound), errors.Is(err, projects.ErrProjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, milestones.CodeMilestoneProjectNotFound,
			"project not found")
	case errors.Is(err, milestones.ErrReleaseNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, milestones.CodeMilestoneReleaseNotFound,
			"the named release does not exist in this project")
	case errors.Is(err, milestones.ErrMilestoneNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, milestones.CodeMilestoneNotFound,
			"milestone not found")
	case errors.Is(err, milestones.ErrStore), errors.Is(err, projects.ErrStore):
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, milestones.CodeMilestoneServiceUnavailable,
			"milestone data is temporarily unavailable")
	default:
		observability.LoggerFromContext(r.Context()).Error("milestone handler: unexpected error", "error", err)
		authhttp.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR",
			"an internal error occurred")
	}
}

// visible resolves the project read gate for the milestone reads: the
// milestone is exactly as visible as the project carrying it.
func (h *handlers) visible(w http.ResponseWriter, r *http.Request, projectID string) bool {
	_, err := h.projects.Get(r.Context(), reader(r), projectID)
	if err != nil {
		writeMilestoneError(w, r, err)
		return false
	}
	return true
}

// handleListMilestones: GET /api/v1/projects/{projectId}/milestones —
// the timeline: occurred_at ascending (the events' dates, the canonical
// kinds' natural progression), creation order breaking date ties. The
// order is the command's contract, never the insertion order.
func (h *handlers) handleListMilestones(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectId")
	if !h.visible(w, r, projectID) {
		return
	}
	list, err := h.cmd.List(r.Context(), projectID)
	if err != nil {
		writeMilestoneError(w, r, err)
		return
	}
	out := make([]milestonePayload, 0, len(list))
	for _, m := range list {
		out = append(out, milestonePayloadFromDomain(m))
	}
	authhttp.WriteJSON(w, http.StatusOK, map[string]any{"milestones": out})
}

// handleCreateMilestone: POST /api/v1/projects/{projectId}/milestones.
// The only write of the surface — V1 has no correction surface, so the
// mux registers no update or delete route. Idempotency-Key (docs/22)
// replays the create it names: a retry returns the first create's row
// instead of duplicating a timeline entry.
func (h *handlers) handleCreateMilestone(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var req createMilestoneRequest
	if !decodeBody(w, r, &req) {
		return
	}
	occurredAt, ok := parseOccurredAt(w, r, req.OccurredAt)
	if !ok {
		return
	}
	var releaseID *string
	if req.ReleaseID != "" {
		releaseID = &req.ReleaseID
	}
	var key *string
	if v := r.Header.Get("Idempotency-Key"); v != "" {
		key = &v
	}
	milestone, err := h.cmd.Create(r.Context(), actor, milestones.CreateMilestoneParams{
		ProjectID:      r.PathValue("projectId"),
		Kind:           domain.MilestoneKind(req.Kind),
		Label:          req.Label,
		OccurredAt:     occurredAt,
		ReleaseID:      releaseID,
		IdempotencyKey: key,
	})
	if err != nil {
		writeMilestoneError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusCreated, milestonePayloadFromDomain(milestone))
}

// handleGetMilestone: GET
// /api/v1/projects/{projectId}/milestones/{milestoneId}.
func (h *handlers) handleGetMilestone(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectId")
	if !h.visible(w, r, projectID) {
		return
	}
	milestone, err := h.cmd.Get(r.Context(), projectID, r.PathValue("milestoneId"))
	if err != nil {
		writeMilestoneError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, milestonePayloadFromDomain(milestone))
}
