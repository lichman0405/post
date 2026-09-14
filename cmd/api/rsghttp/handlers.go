package rsghttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/relations"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/semantics"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// handlers owns the RSG routes. Every handler: resolve the caller (the
// guard put the principal there — writes require it), parse the request,
// call the rsg service, render the payload or the standard error envelope.
type handlers struct {
	svc Service
}

// branchPayload is the client-visible branch shape.
type branchPayload struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	Name        string    `json:"name"`
	Visibility  string    `json:"visibility"`
	Purpose     *string   `json:"purpose"`
	GitRef      string    `json:"git_ref"`
	BaseStateID *string   `json:"base_state_id"`
	Lifecycle   string    `json:"lifecycle_state"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}

func branchPayloadFromDomain(b domain.Branch) branchPayload {
	return branchPayload{
		ID: b.ID, ProjectID: b.ProjectID, Name: b.Name,
		Visibility: string(b.Visibility), Purpose: b.Purpose,
		GitRef: b.GitRef, BaseStateID: b.BaseStateID,
		Lifecycle: string(b.Lifecycle), CreatedBy: b.CreatedBy, CreatedAt: b.CreatedAt,
	}
}

// objectPayload is the client-visible object + current/new version shape.
// The version facts sit at the top level next to the container facts: the
// G3 acceptance contract reads version_id from the create/version/read
// responses directly.
type objectPayload struct {
	ID             string           `json:"id"`
	ProjectID      string           `json:"project_id"`
	ObjectType     string           `json:"object_type"`
	CurrentVersion int              `json:"current_version"`
	VersionID      string           `json:"version_id"`
	VersionNo      int              `json:"version_no"`
	StateID        string           `json:"state_id"`
	Title          string           `json:"title"`
	LifecycleState string           `json:"lifecycle_state"`
	SchemaRef      schemaRefPayload `json:"schema_ref"`
	Payload        json.RawMessage  `json:"payload"`
	Hints          []hintPayload    `json:"hints,omitempty"`
}

type schemaRefPayload struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type hintPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// relationPayload is the client-visible relation + version shape.
type relationPayload struct {
	ID                    string          `json:"id"`
	ProjectID             string          `json:"project_id"`
	VersionID             string          `json:"version_id"`
	VersionNo             int             `json:"version_no"`
	StateID               string          `json:"state_id"`
	RelationType          string          `json:"relation_type"`
	SourceObjectVersionID string          `json:"source_object_version_id"`
	TargetObjectVersionID string          `json:"target_object_version_id"`
	Payload               json.RawMessage `json:"payload"`
}

// decodeBody parses a JSON body (bounded) and reports success — a
// malformed body is answered in place.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if err := dec.Decode(dst); err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, rsg.CodeValidation,
			"request body must be valid JSON")
		return false
	}
	return true
}

// principal resolves the authenticated actor for writes; a missing session
// is answered in place with 401 (the guard enforces the same before
// routing — this is the handler-level backstop).
func principal(w http.ResponseWriter, r *http.Request) (domain.User, bool) {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		authhttp.WriteError(w, r, http.StatusUnauthorized, authn.CodeUnauthenticated,
			"authentication required")
		return domain.User{}, false
	}
	return p.User, true
}

// reader resolves the caller for the visibility-aware read (T0106): the
// same resolution every other project read runs.
func reader(r *http.Request) projects.Reader {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		return projects.Reader{}
	}
	return projects.Reader{UserID: p.User.ID, Authenticated: true}
}

type createBranchRequest struct {
	Name       string  `json:"name"`
	BaseRef    string  `json:"base_ref"`
	Visibility string  `json:"visibility"`
	Purpose    *string `json:"purpose"`
}

// handleCreateBranch: POST /api/v1/projects/{projectId}/branches
func (h *handlers) handleCreateBranch(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var req createBranchRequest
	if !decodeBody(w, r, &req) {
		return
	}
	branch, err := h.svc.CreateBranch(r.Context(), actor, r.PathValue("projectId"), rsg.CreateBranchInput{
		Name:       req.Name,
		BaseRef:    req.BaseRef,
		Visibility: domain.BranchVisibility(req.Visibility),
		Purpose:    req.Purpose,
	})
	if err != nil {
		rsgError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusCreated, branchPayloadFromDomain(branch))
}

type createObjectRequest struct {
	ObjectType string          `json:"object_type"`
	Payload    json.RawMessage `json:"payload"`
	SchemaRef  *string         `json:"schema_ref"`
}

// handleCreateObject: POST /api/v1/projects/{projectId}/branches/{branchId}/objects
func (h *handlers) handleCreateObject(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var req createObjectRequest
	if !decodeBody(w, r, &req) {
		return
	}
	schemaRef := ""
	if req.SchemaRef != nil {
		schemaRef = *req.SchemaRef
	}
	res, err := h.svc.CreateObject(r.Context(), actor, r.PathValue("projectId"), r.PathValue("branchId"), rsg.CreateObjectInput{
		ObjectType: req.ObjectType,
		Payload:    req.Payload,
		SchemaRef:  schemaRef,
	})
	if err != nil {
		rsgError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusCreated, objectPayloadFromResult(res))
}

type createObjectVersionRequest struct {
	ExpectedVersion int             `json:"expected_version"`
	Patch           json.RawMessage `json:"patch"`
}

// handleCreateObjectVersion: POST
// /api/v1/projects/{projectId}/branches/{branchId}/objects/{objectId}:version
// — the route is a remainder wildcard ("{object...}"), the ":version"
// suffix is split off here (ServeMux rejects "{objectId}:version").
func (h *handlers) handleCreateObjectVersion(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	segment := r.PathValue("object")
	if !strings.HasSuffix(segment, ":version") {
		http.NotFound(w, r)
		return
	}
	objectID := strings.TrimSuffix(segment, ":version")
	if objectID == "" {
		http.NotFound(w, r)
		return
	}
	var req createObjectVersionRequest
	if !decodeBody(w, r, &req) {
		return
	}
	res, err := h.svc.CreateObjectVersion(r.Context(), actor, r.PathValue("projectId"), r.PathValue("branchId"), objectID, rsg.CreateObjectVersionInput{
		ExpectedVersion: req.ExpectedVersion,
		Patch:           req.Patch,
	})
	if err != nil {
		rsgError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusCreated, objectPayloadFromVersion(res))
}

type createRelationRequest struct {
	RelationType          string          `json:"relation_type"`
	SourceObjectVersionID string          `json:"source_object_version_id"`
	TargetObjectVersionID string          `json:"target_object_version_id"`
	Payload               json.RawMessage `json:"payload"`
}

// handleCreateRelation: POST
// /api/v1/projects/{projectId}/branches/{branchId}/relations — the OpenAPI
// seed names no request body for this route; the body carries the domain's
// minimal field set (relation_type + the two version-pinned endpoints, plus
// optional metadata).
func (h *handlers) handleCreateRelation(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var req createRelationRequest
	if !decodeBody(w, r, &req) {
		return
	}
	res, err := h.svc.CreateRelation(r.Context(), actor, r.PathValue("projectId"), r.PathValue("branchId"), rsg.CreateRelationInput{
		RelationType:          req.RelationType,
		SourceObjectVersionID: req.SourceObjectVersionID,
		TargetObjectVersionID: req.TargetObjectVersionID,
		Payload:               req.Payload,
	})
	if err != nil {
		rsgError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusCreated, relationPayloadFromResult(res))
}

// handleGetObject: GET
// /api/v1/projects/{projectId}/branches/{branchId}/objects/{objectId} —
// the visibility-aware read (T0106): as visible as its project, existence
// hidden for everyone who may not see it.
func (h *handlers) handleGetObject(w http.ResponseWriter, r *http.Request) {
	res, err := h.svc.GetObject(r.Context(), reader(r), r.PathValue("projectId"), r.PathValue("branchId"), r.PathValue("objectId"))
	if err != nil {
		rsgError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, objectPayloadFromResult(res))
}

// objectPayloadFromResult renders a create/read result (the container row
// plus the current or new version).
func objectPayloadFromResult(res rsg.ObjectResult) objectPayload {
	return objectPayload{
		ID: res.Object.ID, ProjectID: res.Object.ProjectID, ObjectType: res.Object.ObjectType,
		CurrentVersion: res.Object.CurrentVersionNo,
		VersionID:      res.Version.ID, VersionNo: res.Version.VersionNo, StateID: res.Version.StateID,
		Title: res.Version.Title, LifecycleState: string(res.Version.LifecycleState),
		SchemaRef: schemaRefPayload{ID: res.Version.SchemaID, Version: res.Version.SchemaVersion},
		Payload:   res.Version.Payload, Hints: hintsFromSemantics(res.Hints),
	}
}

// objectPayloadFromVersion renders a next-version result.
func objectPayloadFromVersion(res rsg.ObjectVersionResult) objectPayload {
	return objectPayload{
		ID: res.Object.ID, ProjectID: res.Object.ProjectID, ObjectType: res.Object.ObjectType,
		CurrentVersion: res.Version.VersionNo,
		VersionID:      res.Version.ID, VersionNo: res.Version.VersionNo, StateID: res.Version.StateID,
		Title: res.Version.Title, LifecycleState: string(res.Version.LifecycleState),
		SchemaRef: schemaRefPayload{ID: res.Version.SchemaID, Version: res.Version.SchemaVersion},
		Payload:   res.Version.Payload, Hints: hintsFromSemantics(res.Hints),
	}
}

func relationPayloadFromResult(res rsg.RelationResult) relationPayload {
	return relationPayload{
		ID: res.Relation.ID, ProjectID: res.Relation.ProjectID,
		VersionID: res.Version.ID, VersionNo: res.Version.VersionNo, StateID: res.Version.StateID,
		RelationType:          res.Version.RelationType,
		SourceObjectVersionID: res.Version.SourceObjectVersionID,
		TargetObjectVersionID: res.Version.TargetObjectVersionID,
		Payload:               res.Version.Payload,
	}
}

func hintsFromSemantics(hints []semantics.Hint) []hintPayload {
	out := make([]hintPayload, 0, len(hints))
	for _, h := range hints {
		out = append(out, hintPayload{Code: h.Code, Message: h.Message})
	}
	return out
}

// rsgError maps service outcomes to the wire (docs/45: stable codes, no
// dependency detail; a denied write is the same envelope whether the
// object exists or not). Unknown errors are answered with the generic 503.
func rsgError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, projects.ErrProjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, projects.CodeProjectNotFound,
			"project not found")
	case errors.Is(err, rsg.ErrForbidden):
		authhttp.WriteError(w, r, http.StatusForbidden, rsg.CodeForbidden,
			"you are not permitted to write scientific state in this project")
	case errors.Is(err, branches.ErrBranchNotFound) || errors.Is(err, states.ErrBranchNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, states.CodeBranchNotFound,
			"branch not found")
	case errors.Is(err, branches.ErrBranchNameTaken):
		authhttp.WriteError(w, r, http.StatusConflict, branches.CodeBranchNameTaken,
			"branch name already taken")
	case errors.Is(err, branches.ErrPublicBranchInPrivateProject):
		authhttp.WriteError(w, r, http.StatusForbidden, branches.CodePublicBranchInPrivateProject,
			"a private project cannot host a public branch")
	case errors.Is(err, branches.ErrBaseStateNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, branches.CodeBaseStateNotFound,
			"base state not found")
	case errors.Is(err, states.ErrStateNotFound) || errors.Is(err, branches.ErrStateNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, states.CodeStateNotFound,
			"state not found")
	case errors.Is(err, states.ErrStateExists):
		authhttp.WriteError(w, r, http.StatusConflict, states.CodeStateExists,
			"state already exists")
	case errors.As(err, new(*states.StateConflictError)):
		authhttp.WriteError(w, r, http.StatusConflict, states.CodeBranchStateConflict,
			"branch state conflict — re-read the branch head and retry")
	case errors.As(err, new(*states.BranchNotActiveError)) || errors.As(err, new(*branches.NotActiveError)) || errors.Is(err, branches.ErrBranchNotActive):
		authhttp.WriteError(w, r, http.StatusConflict, states.CodeBranchNotActive,
			"branch lifecycle is not active")
	case errors.Is(err, sciobjects.ErrObjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, sciobjects.CodeObjectNotFound,
			"object not found")
	case errors.Is(err, sciobjects.ErrVersionNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, sciobjects.CodeVersionNotFound,
			"object version not found")
	case errors.As(err, new(*sciobjects.VersionConflictError)):
		authhttp.WriteError(w, r, http.StatusConflict, sciobjects.CodeVersionConflict,
			"expected_version mismatch — re-read the object and retry")
	case errors.Is(err, relations.ErrRelationNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, relations.CodeRelationNotFound,
			"relation not found")
	case errors.Is(err, relations.ErrRelationVersionNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, relations.CodeRelationVersionNotFound,
			"relation version not found")
	case errors.As(err, new(*relations.ReferencedVersionNotFoundError)) || errors.Is(err, relations.ErrReferencedVersionNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, relations.CodeObjectVersionNotFound,
			"referenced object version not found")
	case errors.As(err, new(*relations.UnknownRelationTypeError)):
		authhttp.WriteError(w, r, http.StatusBadRequest, relations.CodeRSGValidationFailed,
			"relation type is not in the V1 catalog")
	case errors.As(err, new(*rsgvalidation.GateBlockedError)):
		var blocked *rsgvalidation.GateBlockedError
		errors.As(err, &blocked)
		authhttp.WriteError(w, r, http.StatusUnprocessableEntity, blocked.Code(),
			"the write was refused: the commit's validation gate is blocked (run :validate for the full report)")
	case errors.Is(err, rsg.ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, rsg.CodeValidation, err.Error())
	default:
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, rsg.CodeUnavailable,
			"service unavailable")
	}
}
