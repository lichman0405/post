// Package templateshttp owns the official project template routes
// (T0214): list the catalog and create a project FROM a template — the
// template's defaults are applied once (project presets, schema profiles,
// the first policy version, the initial research-map questions) and never
// control the project afterwards.
//
// Routes (under the guarded /api/v1 subtree, so every write is session +
// CSRF protected by construction):
//
//	GET  /api/v1/templates
//	GET  /api/v1/templates/{templateId}?version=
//	POST /api/v1/templates/{templateId}/instantiate
//	GET  /api/v1/projects/{projectId}/template-instantiation
//
// Catalog reads are public (templates carry no project state — there is
// nothing to hide); instantiating runs the ordinary create_project
// authorization inside the projects service with the actor.
package templateshttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/templates"
	"github.com/lichman0405/post/internal/domain"
)

// Service is the slice of the templates application service the routes
// need.
type Service interface {
	List() ([]domain.ProjectTemplate, error)
	Get(id, version string) (domain.ProjectTemplate, error)
	Instantiate(ctx context.Context, actor domain.User, in templates.InstantiateInput) (templates.Instantiation, error)
	GetInstantiation(ctx context.Context, r projects.Reader, projectID string) (domain.TemplateInstantiation, error)
}

// handlers owns the template routes.
type handlers struct {
	svc Service
}

// --- payloads ---

// schemaRefPayload is the client-visible {id, version} pin.
type schemaRefPayload struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// templateSchemaPayload renders one schema profile definition.
type templateSchemaPayload struct {
	Name       string           `json:"name"`
	Base       schemaRefPayload `json:"base"`
	Properties map[string]any   `json:"properties"`
	Required   []string         `json:"required"`
}

// templateQuestionPayload renders one research-map seed.
type templateQuestionPayload struct {
	Statement string                    `json:"statement"`
	Purpose   string                    `json:"purpose"`
	Children  []templateQuestionPayload `json:"children"`
}

// templatePayload is the client-visible template shape: the defaults it
// would apply, minus the review rules' raw values being re-rendered as
// JSON.
type templatePayload struct {
	ID          string                    `json:"id"`
	Version     string                    `json:"version"`
	Name        string                    `json:"name"`
	Description string                    `json:"description"`
	Project     templateProjectDefaults   `json:"project"`
	Schemas     []templateSchemaPayload   `json:"schemas"`
	Review      json.RawMessage           `json:"review"`
	Map         []templateQuestionPayload `json:"map"`
}

// templateProjectDefaults is the project-row preset part of the payload.
type templateProjectDefaults struct {
	Name       string `json:"name"`
	Purpose    string `json:"purpose"`
	Visibility string `json:"visibility"`
}

func templatePayloadFromDomain(t domain.ProjectTemplate) templatePayload {
	out := templatePayload{
		ID:          t.ID,
		Version:     t.Version,
		Name:        t.Name,
		Description: t.Description,
		Project: templateProjectDefaults{
			Name:       t.Project.Name,
			Purpose:    t.Project.Purpose,
			Visibility: string(t.Project.Visibility),
		},
		Schemas: make([]templateSchemaPayload, 0, len(t.Schemas)),
		Review:  json.RawMessage("{}"),
		Map:     make([]templateQuestionPayload, 0, len(t.Map)),
	}
	if len(t.Review.Rules) > 0 {
		raw, err := json.Marshal(t.Review.Rules)
		if err == nil {
			out.Review = raw
		}
	}
	for _, p := range t.Schemas {
		props := p.Properties
		if props == nil {
			props = map[string]any{}
		}
		req := p.Required
		if req == nil {
			req = []string{}
		}
		out.Schemas = append(out.Schemas, templateSchemaPayload{
			Name:       p.Name,
			Base:       schemaRefPayload{ID: p.Base.ID, Version: p.Base.Version},
			Properties: props,
			Required:   req,
		})
	}
	for _, q := range t.Map {
		out.Map = append(out.Map, templateQuestionFromDomain(q))
	}
	return out
}

func templateQuestionFromDomain(q domain.TemplateQuestion) templateQuestionPayload {
	out := templateQuestionPayload{Statement: q.Statement, Purpose: q.Purpose, Children: []templateQuestionPayload{}}
	for _, c := range q.Children {
		out.Children = append(out.Children, templateQuestionFromDomain(c))
	}
	return out
}

// projectPayload/membershipPayload mirror the projectshttp shapes (the
// create-from-template response answers the same project contract).
type projectPayload struct {
	ID              string    `json:"id"`
	OrganizationID  *string   `json:"organization_id"`
	ProgramID       *string   `json:"program_id"`
	Slug            string    `json:"slug"`
	Name            string    `json:"name"`
	Purpose         string    `json:"purpose"`
	ActivityStatus  string    `json:"activity_status"`
	Visibility      string    `json:"visibility"`
	MainFrozen      bool      `json:"main_frozen"`
	ProvisionStatus string    `json:"provision_status"`
	CreatedBy       string    `json:"created_by"`
	CreatedAt       time.Time `json:"created_at"`
}

func projectPayloadFromDomain(p domain.Project) projectPayload {
	return projectPayload{
		ID:              p.ID,
		OrganizationID:  p.OrganizationID,
		ProgramID:       p.ProgramID,
		Slug:            p.Slug,
		Name:            p.Name,
		Purpose:         p.Purpose,
		ActivityStatus:  p.ActivityStatus,
		Visibility:      string(p.Visibility),
		MainFrozen:      p.MainFrozen,
		ProvisionStatus: string(p.ProvisionStatus),
		CreatedBy:       p.CreatedBy,
		CreatedAt:       p.CreatedAt,
	}
}

type membershipPayload struct {
	ProjectID string `json:"project_id"`
	UserID    string `json:"user_id"`
	Role      string `json:"role"`
}

func membershipPayloadFromDomain(m domain.ProjectMembership) membershipPayload {
	return membershipPayload{ProjectID: m.ProjectID, UserID: m.UserID, Role: string(m.Role)}
}

// stepPayload renders one applied-or-failed step outcome: error "" means
// the step applied.
type stepPayload struct {
	Error string `json:"error,omitempty"`
}

func stepPayloadFromDomain(s templates.StepOutcome) stepPayload {
	return stepPayload{Error: s.Error}
}

// appliedProfilePayload renders one schema profile registration outcome.
type appliedProfilePayload struct {
	SchemaID string `json:"schema_id"`
	Version  string `json:"version"`
	Error    string `json:"error,omitempty"`
}

// appliedQuestionPayload renders one seeded question outcome.
type appliedQuestionPayload struct {
	ObjectID  string `json:"object_id,omitempty"`
	Statement string `json:"statement"`
	Error     string `json:"error,omitempty"`
}

// appliedMapPayload renders the research-map seeding outcome.
type appliedMapPayload struct {
	BranchID  string                   `json:"branch_id,omitempty"`
	Questions []appliedQuestionPayload `json:"questions"`
	Error     string                   `json:"error,omitempty"`
}

// appliedPayload renders the per-default application report: the record
// step, the profiles, the policy and the map seeds — each either applied
// or failed. A failure is rendered exactly as the service reported it: the
// step's name and the failure class, never the owning service's error text
// (docs/45: stable codes, no dependency detail — the cause goes to the
// service log). A failed default never fakes a failed creation; the
// project is the primary fact.
type appliedPayload struct {
	Recorded *stepPayload            `json:"recorded"`
	Profiles []appliedProfilePayload `json:"profiles"`
	Policy   *stepPayload            `json:"policy,omitempty"`
	Map      *appliedMapPayload      `json:"map,omitempty"`
}

func appliedPayloadFromDomain(a templates.AppliedReport) appliedPayload {
	out := appliedPayload{Profiles: []appliedProfilePayload{}}
	if a.Recorded != nil {
		s := stepPayloadFromDomain(*a.Recorded)
		out.Recorded = &s
	}
	for _, p := range a.Profiles {
		out.Profiles = append(out.Profiles, appliedProfilePayload{
			SchemaID: p.SchemaID,
			Version:  p.Version,
			Error:    p.Error,
		})
	}
	if a.Policy != nil {
		s := stepPayloadFromDomain(*a.Policy)
		out.Policy = &s
	}
	if a.Map != nil {
		m := appliedMapPayload{Questions: []appliedQuestionPayload{}, Error: a.Map.Error}
		m.BranchID = a.Map.BranchID
		for _, q := range a.Map.Questions {
			m.Questions = append(m.Questions, appliedQuestionPayload{
				ObjectID:  q.ObjectID,
				Statement: q.Statement,
				Error:     q.Error,
			})
		}
		out.Map = &m
	}
	return out
}

// instantiationPayload renders the provenance record (记录 template
// id/version).
type instantiationPayload struct {
	ID              string    `json:"id"`
	ProjectID       string    `json:"project_id"`
	TemplateID      string    `json:"template_id"`
	TemplateVersion string    `json:"template_version"`
	TemplateName    string    `json:"template_name"`
	CreatedBy       string    `json:"created_by"`
	CreatedAt       time.Time `json:"created_at"`
}

func instantiationPayloadFromDomain(r domain.TemplateInstantiation) instantiationPayload {
	return instantiationPayload{
		ID:              r.ID,
		ProjectID:       r.ProjectID,
		TemplateID:      r.TemplateID,
		TemplateVersion: r.TemplateVersion,
		TemplateName:    r.TemplateName,
		CreatedBy:       r.CreatedBy,
		CreatedAt:       r.CreatedAt,
	}
}

// --- handlers ---

// handleList: GET /api/v1/templates — the official catalog, newest
// version per template id, id-sorted. Public read (the catalog carries no
// project state).
func (h *handlers) handleList(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.List()
	if err != nil {
		h.templateError(w, r, err)
		return
	}
	out := make([]templatePayload, 0, len(list))
	for _, t := range list {
		out = append(out, templatePayloadFromDomain(t))
	}
	authhttp.WriteJSON(w, http.StatusOK, map[string]any{"templates": out})
}

// handleGet: GET /api/v1/templates/{templateId}?version= — one template;
// version "" resolves the latest. Public read.
func (h *handlers) handleGet(w http.ResponseWriter, r *http.Request) {
	t, err := h.svc.Get(r.PathValue("templateId"), r.URL.Query().Get("version"))
	if err != nil {
		h.templateError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, templatePayloadFromDomain(t))
}

// instantiateRequest is the create-from-template body: the template
// version pin ("" = latest) and the ordinary project fields, each
// optional here — the template's defaults fill what the caller leaves
// unset.
type instantiateRequest struct {
	TemplateVersion string  `json:"template_version"`
	Slug            string  `json:"slug"`
	Name            string  `json:"name"`
	Purpose         string  `json:"purpose"`
	Visibility      string  `json:"visibility"`
	OrganizationID  *string `json:"organization_id"`
	ProgramID       *string `json:"program_id"`
}

// handleInstantiate: POST /api/v1/templates/{templateId}/instantiate —
// creates the project from the template and applies its defaults once.
// The response answers 201 with the project, the creator membership, the
// provenance record and the per-default application report; a failed
// default appears in the report, never as a faked creation failure.
func (h *handlers) handleInstantiate(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var req instantiateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	out, err := h.svc.Instantiate(r.Context(), actor, templates.InstantiateInput{
		TemplateID:      r.PathValue("templateId"),
		TemplateVersion: req.TemplateVersion,
		OrganizationID:  req.OrganizationID,
		ProgramID:       req.ProgramID,
		Slug:            req.Slug,
		Name:            req.Name,
		Purpose:         req.Purpose,
		Visibility:      domain.ProjectVisibility(req.Visibility),
	})
	if err != nil {
		h.templateError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusCreated, map[string]any{
		"template":   templatePayloadFromDomain(out.Template),
		"project":    projectPayloadFromDomain(out.Project),
		"membership": membershipPayloadFromDomain(out.Membership),
		"record":     instantiationPayloadFromDomain(out.Instantiation),
		"applied":    appliedPayloadFromDomain(out.Applied),
	})
}

// handleGetInstantiation: GET /api/v1/projects/{projectId}/
// template-instantiation — the project's recorded template origin. The
// read is exactly as visible as the project: the projects service's own
// Get (with the caller's principal, anonymous when none) runs first, so a
// denied read answers the existence-hiding 404 and a project without a
// template origin answers 404 as well (nothing is disclosed).
func (h *handlers) handleGetInstantiation(w http.ResponseWriter, r *http.Request) {
	var reader projects.Reader
	if p, ok := authhttp.PrincipalFrom(r.Context()); ok {
		reader = projects.Reader{UserID: p.User.ID, Authenticated: true}
	}
	rec, err := h.svc.GetInstantiation(r.Context(), reader, r.PathValue("projectId"))
	if err != nil {
		h.templateError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, instantiationPayloadFromDomain(rec))
}

// principal resolves the authenticated actor for writes (the guard put
// the principal there; this is the handler-level backstop, same as the
// projects surface).
func principal(w http.ResponseWriter, r *http.Request) (domain.User, bool) {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		authhttp.WriteError(w, r, http.StatusUnauthorized, authn.CodeUnauthenticated,
			"authentication required")
		return domain.User{}, false
	}
	return p.User, true
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

// templateError maps Service errors to the wire (docs/45: stable codes,
// no dependency detail). The projects service's create outcomes reuse
// the projects surface's codes and messages — a create-from-template
// answers the same stable contract a plain create answers.
func (h *handlers) templateError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, templates.ErrTemplateNotFound),
		errors.Is(err, templates.ErrInstantiationNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, templates.CodeTemplateNotFound,
			"template not found")
	case errors.Is(err, templates.ErrInstantiationExists):
		authhttp.WriteError(w, r, http.StatusConflict, templates.CodeValidation,
			"the project already records a template instantiation")
	case errors.Is(err, templates.ErrValidation),
		errors.Is(err, projects.ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, templates.CodeValidation, err.Error())
	case errors.Is(err, projects.ErrProjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, projects.CodeProjectNotFound,
			"project not found")
	case errors.Is(err, projects.ErrSlugTaken):
		authhttp.WriteError(w, r, http.StatusConflict, projects.CodeProjectSlugTaken,
			"a project with this slug already exists")
	case errors.Is(err, projects.ErrMemberNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, projects.CodeProjectMembershipNotFound,
			"you are not a member of this project")
	case errors.Is(err, projects.ErrForbidden):
		authhttp.WriteError(w, r, http.StatusForbidden, projects.CodeProjectForbidden,
			"you are not an active member of this organization")
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
	case errors.Is(err, templates.ErrStore):
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, templates.CodeUnavailable,
			"template data is temporarily unavailable")
	default:
		authhttp.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR",
			"an internal error occurred")
	}
}
