package conflicthttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/resolutions"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/conflict"
)

// handlers owns the conflict resolution routes. Every handler: parse,
// call the application service, render the payload or the standard error
// envelope.
type handlers struct {
	viewer   Viewer
	saver    Saver
	projects ProjectReader
}

// conflictViewPayload is the wire shape of the GET conflicts answer: the
// detector report (diff + verdicts — the changes carry the base/A/B
// values), the per-side evidence context and the recorded decisions.
type conflictViewPayload struct {
	Report      *conflict.Report             `json:"report"`
	Evidence    []resolutions.ObjectEvidence `json:"evidence"`
	Resolutions []resolutionPayload          `json:"resolutions"`
}

// resolutionPayload is the wire shape of one recorded decision.
type resolutionPayload struct {
	ID            string    `json:"id"`
	ProjectID     string    `json:"project_id"`
	BaseStateID   string    `json:"base_state_id"`
	SourceStateID string    `json:"source_state_id"`
	TargetStateID string    `json:"target_state_id"`
	TargetKind    string    `json:"target_kind"`
	TargetID      string    `json:"target_id"`
	Code          string    `json:"code"`
	Fields        []string  `json:"fields"`
	PayloadKeys   []string  `json:"payload_keys"`
	OtherObjectID *string   `json:"other_object_id"`
	Kind          string    `json:"kind"`
	Note          string    `json:"note"`
	DecidedBy     string    `json:"decided_by"`
	DecidedAt     time.Time `json:"decided_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func resolutionPayloadFromDomain(r domain.ConflictResolution) resolutionPayload {
	return resolutionPayload{
		ID: r.ID, ProjectID: r.ProjectID,
		BaseStateID: r.BaseStateID, SourceStateID: r.SourceStateID, TargetStateID: r.TargetStateID,
		TargetKind: string(r.TargetKind), TargetID: r.TargetID, Code: r.Code,
		Fields: r.Fields, PayloadKeys: r.PayloadKeys, OtherObjectID: r.OtherObjectID,
		Kind: string(r.Kind), Note: r.Note, DecidedBy: r.DecidedBy,
		DecidedAt: r.DecidedAt, UpdatedAt: r.UpdatedAt,
	}
}

func resolutionPayloads(rows []domain.ConflictResolution) []resolutionPayload {
	out := make([]resolutionPayload, 0, len(rows))
	for _, r := range rows {
		out = append(out, resolutionPayloadFromDomain(r))
	}
	return out
}

// saveResolutionsRequest is the wire body of the PUT resolutions write:
// the pinned triple and the decisions.
type saveResolutionsRequest struct {
	BaseStateID   string            `json:"base_state_id"`
	SourceStateID string            `json:"source_state_id"`
	TargetStateID string            `json:"target_state_id"`
	Resolutions   []decisionPayload `json:"resolutions"`
}

// decisionPayload is one submitted decision: the classifier key of the
// conflict it addresses plus the chosen human decision. No field is a
// computed value — the wire carries exactly what the human chose.
type decisionPayload struct {
	TargetKind    string   `json:"target_kind"`
	TargetID      string   `json:"target_id"`
	Code          string   `json:"code"`
	Fields        []string `json:"fields"`
	PayloadKeys   []string `json:"payload_keys"`
	OtherObjectID *string  `json:"other_object_id"`
	Kind          string   `json:"kind"`
	Note          string   `json:"note"`
}

func (d decisionPayload) toDomain() resolutions.Decision {
	return resolutions.Decision{
		TargetKind:    domain.ConflictResolutionTargetKind(d.TargetKind),
		TargetID:      d.TargetID,
		Code:          d.Code,
		Fields:        d.Fields,
		PayloadKeys:   d.PayloadKeys,
		OtherObjectID: d.OtherObjectID,
		Kind:          domain.ResolutionKind(d.Kind),
		Note:          d.Note,
	}
}

// reader resolves the caller for the visibility gate (T0106): a session
// makes them authenticated, its absence makes them anonymous. The routes
// are guarded, so anonymous writes are answered 401 by the guard before
// routing — the Reader here is the product-level backstop every other
// project read runs.
func reader(r *http.Request) projects.Reader {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		return projects.Reader{}
	}
	return projects.Reader{UserID: p.User.ID, Authenticated: true}
}

// triple decodes the three state ids of a request (the query of the read,
// the body of the write) and reports whether they are all present.
type triple struct {
	base, source, target string
}

func tripleFromQuery(r *http.Request) (triple, bool) {
	q := r.URL.Query()
	t := triple{base: q.Get("base_state_id"), source: q.Get("source_state_id"), target: q.Get("target_state_id")}
	return t, t.complete()
}

func (t triple) complete() bool { return t.base != "" && t.source != "" && t.target != "" }

// projectGate runs the project read boundary first (T0106 read
// semantics): the conflicts of a project are exactly as visible as the
// project, so the caller's ability to read the project is decided before
// any report is computed. A denied read answers the same
// existence-hiding 404 as every other project read. Missing wiring fails
// closed.
func (h *handlers) projectGate(w http.ResponseWriter, r *http.Request, projectID string) bool {
	if h.projects == nil {
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, resolutions.CodeStore,
			"conflict service unavailable")
		return false
	}
	if _, err := h.projects.Get(r.Context(), reader(r), projectID); err != nil {
		switch {
		case errors.Is(err, projects.ErrProjectNotFound):
			authhttp.WriteError(w, r, http.StatusNotFound, projects.CodeProjectNotFound,
				"project not found")
		default:
			authhttp.WriteError(w, r, http.StatusServiceUnavailable, resolutions.CodeStore,
				"conflict service unavailable")
		}
		return false
	}
	return true
}

// handleGetConflicts: GET /api/v1/projects/{projectId}/conflicts
func (h *handlers) handleGetConflicts(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectId")
	if !h.projectGate(w, r, projectID) {
		return
	}
	t, ok := tripleFromQuery(r)
	if !ok {
		authhttp.WriteError(w, r, http.StatusBadRequest, resolutions.CodeValidation,
			"base_state_id, source_state_id and target_state_id are required query parameters")
		return
	}
	if h.viewer == nil {
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, resolutions.CodeStore,
			"conflict service unavailable")
		return
	}
	view, err := h.viewer.View(r.Context(), projectID, t.base, t.source, t.target)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	if view == nil || view.Report == nil {
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, resolutions.CodeStore,
			"conflict service unavailable")
		return
	}
	writeJSON(w, http.StatusOK, conflictViewPayload{
		Report:      view.Report,
		Evidence:    view.Evidence,
		Resolutions: resolutionPayloads(view.Resolutions),
	})
}

// handlePutResolutions: PUT /api/v1/projects/{projectId}/resolutions
func (h *handlers) handlePutResolutions(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectId")
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		authhttp.WriteError(w, r, http.StatusUnauthorized, authn.CodeUnauthenticated,
			"authentication required")
		return
	}
	if !h.projectGate(w, r, projectID) {
		return
	}
	var req saveResolutionsRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := dec.Decode(&req); err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, resolutions.CodeValidation,
			"request body must be valid JSON with base_state_id, source_state_id, target_state_id and resolutions")
		return
	}
	in := resolutions.SaveInput{
		ProjectID:     projectID,
		BaseStateID:   req.BaseStateID,
		SourceStateID: req.SourceStateID,
		TargetStateID: req.TargetStateID,
		Decisions:     make([]resolutions.Decision, 0, len(req.Resolutions)),
	}
	for _, d := range req.Resolutions {
		in.Decisions = append(in.Decisions, d.toDomain())
	}
	if h.saver == nil {
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, resolutions.CodeStore,
			"conflict service unavailable")
		return
	}
	plan, err := h.saver.Save(r.Context(), p.User, in)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]resolutionPayload{"resolutions": resolutionPayloads(plan)})
}

// writeServiceError maps the service outcomes onto the wire (docs/45):
// stable codes, never dependency detail.
func (h *handlers) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, resolutions.ErrForbidden):
		authhttp.WriteError(w, r, http.StatusForbidden, resolutions.CodeForbidden,
			"you are not allowed to resolve conflicts in this project")
	case errors.Is(err, resolutions.ErrConflictNotFound):
		authhttp.WriteError(w, r, http.StatusConflict, resolutions.CodeConflictNotFound,
			"a resolution names a conflict the report does not contain — re-read the conflict report and retry")
	case errors.Is(err, resolutions.ErrValidation), errors.Is(err, diffs.ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, resolutions.CodeValidation, err.Error())
	case errors.Is(err, diffs.ErrStateNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, "STATE_NOT_FOUND",
			"one of the three states does not exist")
	default:
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, resolutions.CodeStore,
			"conflict service unavailable")
	}
}

// writeJSON renders one payload; the canonical report bytes are the
// detector's own field order (json.Marshal of the report struct).
func writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
