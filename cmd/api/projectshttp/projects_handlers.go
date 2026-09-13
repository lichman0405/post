package projectshttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
)

// The project HTTP surface. Every handler: resolve the caller (the guard
// put the principal there when a session exists — writes require it, reads
// resolve to an anonymous reader when it does not, T0106), parse the
// request, call the Service, render the payload or the standard error
// envelope.

// handlers owns the project routes.
type handlers struct {
	svc *projects.Service
}

// projectPayload is the client-visible project shape.
type projectPayload struct {
	ID                      string    `json:"id"`
	OrganizationID          *string   `json:"organization_id"`
	ProgramID               *string   `json:"program_id"`
	Slug                    string    `json:"slug"`
	Name                    string    `json:"name"`
	Purpose                 string    `json:"purpose"`
	ActivityStatus          string    `json:"activity_status"`
	Visibility              string    `json:"visibility"`
	MainFrozen              bool      `json:"main_frozen"`
	GitRepositoryExternalID *string   `json:"git_repository_external_id"`
	ProvisionStatus         string    `json:"provision_status"`
	CreatedBy               string    `json:"created_by"`
	CreatedAt               time.Time `json:"created_at"`
}

func projectPayloadFromDomain(p domain.Project) projectPayload {
	return projectPayload{
		ID:                      p.ID,
		OrganizationID:          p.OrganizationID,
		ProgramID:               p.ProgramID,
		Slug:                    p.Slug,
		Name:                    p.Name,
		Purpose:                 p.Purpose,
		ActivityStatus:          p.ActivityStatus,
		Visibility:              string(p.Visibility),
		MainFrozen:              p.MainFrozen,
		GitRepositoryExternalID: p.GitRepositoryExternalID,
		ProvisionStatus:         string(p.ProvisionStatus),
		CreatedBy:               p.CreatedBy,
		CreatedAt:               p.CreatedAt,
	}
}

// membershipPayload is the client-visible project membership shape.
type membershipPayload struct {
	ProjectID string    `json:"project_id"`
	UserID    string    `json:"user_id"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

func membershipPayloadFromDomain(m domain.ProjectMembership) membershipPayload {
	return membershipPayload{
		ProjectID: m.ProjectID,
		UserID:    m.UserID,
		Role:      string(m.Role),
		CreatedAt: m.CreatedAt,
	}
}

// decodeBody parses a JSON body (bounded; unknown fields ignored per
// contract) and reports success — a malformed body is answered in place.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if err := dec.Decode(dst); err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, projects.CodeValidationFailed,
			"request body must be valid JSON")
		return false
	}
	return true
}

// principal resolves the authenticated actor for writes; a missing
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

// reader resolves the caller for visibility-aware reads (T0106): a
// session makes them authenticated, its absence makes them anonymous.
// Reads never 401 — anonymous callers may read public projects; the
// service hides private ones behind the existence-hiding 404.
func reader(r *http.Request) projects.Reader {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		return projects.Reader{}
	}
	return projects.Reader{UserID: p.User.ID, Authenticated: true}
}

type createProjectRequest struct {
	Slug           string  `json:"slug"`
	Name           string  `json:"name"`
	Purpose        string  `json:"purpose"`
	Visibility     string  `json:"visibility"`
	OrganizationID *string `json:"organization_id"`
	ProgramID      *string `json:"program_id"`
}

// handleCreate: POST /api/v1/projects — the actor becomes owner, the
// project comes back provision-pending.
func (h *handlers) handleCreate(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var req createProjectRequest
	if !decodeBody(w, r, &req) {
		return
	}
	project, membership, err := h.svc.Create(r.Context(), actor, projects.CreateProjectInput{
		OrganizationID: req.OrganizationID,
		ProgramID:      req.ProgramID,
		Slug:           req.Slug,
		Name:           req.Name,
		Purpose:        req.Purpose,
		Visibility:     domain.ProjectVisibility(req.Visibility),
	})
	if err != nil {
		h.projectError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusCreated, map[string]any{
		"project":    projectPayloadFromDomain(project),
		"membership": membershipPayloadFromDomain(membership),
	})
}

// handleList: GET /api/v1/projects — the projects the caller may see,
// newest first: every public project plus the caller's own (T0106
// visibility filtering; anonymous callers get the public list only —
// a private project never appears for a non-member).
func (h *handlers) handleList(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.List(r.Context(), reader(r))
	if err != nil {
		h.projectError(w, r, err)
		return
	}
	out := make([]projectPayload, 0, len(list))
	for _, p := range list {
		out = append(out, projectPayloadFromDomain(p))
	}
	authhttp.WriteJSON(w, http.StatusOK, map[string]any{"projects": out})
}

// handleGet: GET /api/v1/projects/{projectId} — visibility-aware read
// (T0106): public projects are readable by anyone (anonymous included);
// private projects answer the existence-hiding 404 for everyone but
// members.
func (h *handlers) handleGet(w http.ResponseWriter, r *http.Request) {
	project, err := h.svc.Get(r.Context(), reader(r), r.PathValue("projectId"))
	if err != nil {
		h.projectError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, projectPayloadFromDomain(project))
}

// projectError maps Service errors to the wire (docs/45: stable codes, no
// dependency detail). Unknown errors are logged with detail and answered
// with a generic envelope.
func (h *handlers) projectError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, projects.ErrProjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, projects.CodeProjectNotFound,
			"project not found")
	case errors.Is(err, projects.ErrSlugTaken):
		authhttp.WriteError(w, r, http.StatusConflict, projects.CodeProjectSlugTaken,
			"a project with this slug already exists")
	case errors.Is(err, projects.ErrForbidden):
		authhttp.WriteError(w, r, http.StatusForbidden, projects.CodeProjectForbidden,
			"you are not an active member of this organization")
	case errors.Is(err, projects.ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, projects.CodeValidationFailed, err.Error())
	case errors.Is(err, projects.ErrOrgNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, projects.CodeOrgNotFound,
			"organization not found")
	case errors.Is(err, projects.ErrOrgDeactivated):
		authhttp.WriteError(w, r, http.StatusConflict, projects.CodeOrgDeactivated,
			"the organization is deactivated")
	case errors.Is(err, projects.ErrProgramNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, projects.CodeProgramNotFound,
			"program not found")
	case errors.Is(err, projects.ErrProgramOrgMismatch):
		authhttp.WriteError(w, r, http.StatusBadRequest, projects.CodeProgramOrgMismatch,
			"the program does not belong to the project's organization")
	case errors.Is(err, projects.ErrStore):
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, projects.CodeServiceUnavailable,
			"project data is temporarily unavailable")
	default:
		observability.LoggerFromContext(r.Context()).Error("projects handler: unexpected error", "error", err)
		authhttp.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR",
			"an internal error occurred")
	}
}
