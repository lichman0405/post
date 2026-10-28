package rsghttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
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
//
// provenance and evidence are the two graph reads the object detail page's
// Provenance and Evidence tabs render (T0507). They are separate ports
// because they ARE separate graphs (CLAUDE.md §9 invariant 10): different
// tables, different relation vocabularies, different services, and neither
// one is derivable from the other. A nil reader is not an error — the page
// renders that graph as unreadable rather than inventing one (see
// graphStateUnavailable in graph.go).
type handlers struct {
	svc        Service
	provenance ProvenanceReader
	evidence   EvidenceReader
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

type createEvidenceAssertionRequest struct {
	// TargetVersionRef and EvidenceVersionRef are the schema's two version
	// pins (specs/schemas/evidence-assertion.schema.json); the platform's
	// optional `object_version:` prefix is stripped here, exactly as the
	// publish surface strips it from knowledge_version_ref.
	TargetVersionRef   string          `json:"target_version_ref"`
	EvidenceVersionRef string          `json:"evidence_version_ref"`
	Relation           string          `json:"relation"`
	EvidenceType       string          `json:"evidence_type"`
	Scope              json.RawMessage `json:"scope"`
	Directness         string          `json:"directness"`
	InferenceNature    string          `json:"inference_nature"`
	ReasoningNote      string          `json:"reasoning_note"`
}

// evidenceAssertionPayload is the client-visible assertion shape: the stored
// row's own facts, including the two axes the write derived
// (evidence_origin, visibility) and the review state the row was born with.
// Nothing here is client input echoed back.
type evidenceAssertionPayload struct {
	ID                      string          `json:"id"`
	ProjectID               string          `json:"project_id"`
	StateID                 string          `json:"state_id"`
	TargetObjectVersionID   string          `json:"target_object_version_id"`
	EvidenceObjectVersionID string          `json:"evidence_object_version_id"`
	Relation                string          `json:"relation"`
	EvidenceType            string          `json:"evidence_type"`
	Scope                   json.RawMessage `json:"scope"`
	Directness              string          `json:"directness"`
	InferenceNature         string          `json:"inference_nature"`
	ReasoningNote           string          `json:"reasoning_note"`
	ReviewState             string          `json:"review_state"`
	EvidenceOrigin          string          `json:"evidence_origin"`
	Visibility              string          `json:"visibility"`
	CreatedBy               string          `json:"created_by"`
	CreatedAt               time.Time       `json:"created_at"`
	Hints                   []hintPayload   `json:"hints,omitempty"`
}

// handleCreateEvidenceAssertion: POST
// /api/v1/projects/{projectId}/branches/{branchId}/evidence-assertions — the
// OpenAPI route for an evidence assertion (specs/api/openapi.yaml declares no
// request body for it; the body carries the evidence-assertion schema's own
// field names, which is the vocabulary specs/mcp/tools.json already gives the
// operation). The write path is the RSG service's evidence command; the two
// derived axes (evidence_origin, visibility) are NOT accepted from the
// client — there is no field for them here, deliberately.
func (h *handlers) handleCreateEvidenceAssertion(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var req createEvidenceAssertionRequest
	if !decodeBody(w, r, &req) {
		return
	}
	res, err := h.svc.CreateEvidenceAssertion(r.Context(), actor, r.PathValue("projectId"), r.PathValue("branchId"), rsg.CreateEvidenceAssertionInput{
		TargetVersionRef:   versionRefID(req.TargetVersionRef),
		EvidenceVersionRef: versionRefID(req.EvidenceVersionRef),
		Relation:           req.Relation,
		EvidenceType:       req.EvidenceType,
		Scope:              req.Scope,
		Directness:         req.Directness,
		InferenceNature:    req.InferenceNature,
		ReasoningNote:      req.ReasoningNote,
	})
	if err != nil {
		rsgError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusCreated, evidenceAssertionPayloadFromResult(res))
}

// versionRefID strips the platform's `object_version:` ref prefix if the
// caller sent one. It does not validate: the command refuses a reference that
// is not a version uuid, and doing the shape check in two places is how the
// two come to disagree about what a legal reference is (the publish surface's
// versionID does the same).
func versionRefID(ref string) string {
	ref = strings.TrimSpace(ref)
	return strings.TrimPrefix(ref, "object_version:")
}

// evidenceAssertionPayloadFromResult renders the stored assertion (the
// service returns the row as the database wrote it) plus the advisory hints.
func evidenceAssertionPayloadFromResult(res rsg.EvidenceAssertionResult) evidenceAssertionPayload {
	row := res.Assertion
	out := evidenceAssertionPayload{
		ID:                      row.ID,
		ProjectID:               row.ProjectID,
		StateID:                 row.StateID,
		TargetObjectVersionID:   row.TargetObjectVersionID,
		EvidenceObjectVersionID: row.EvidenceObjectVersionID,
		Relation:                row.RelationType,
		EvidenceType:            row.EvidenceType,
		Scope:                   row.Scope,
		Directness:              row.Directness,
		InferenceNature:         row.InferenceNature,
		ReasoningNote:           row.ReasoningNote,
		ReviewState:             row.ReviewState,
		EvidenceOrigin:          row.EvidenceOrigin,
		Visibility:              row.Visibility,
		CreatedBy:               row.CreatedBy,
		CreatedAt:               row.CreatedAt.UTC(),
	}
	for _, hint := range res.Hints {
		out.Hints = append(out.Hints, hintPayload{Code: hint.Code, Message: hint.Message})
	}
	return out
}

// handleGetObject: GET
// /api/v1/projects/{projectId}/branches/{branchId}/objects/{objectId} —
// the visibility-aware read (T0106): as visible as its project, existence
// hidden for everyone who may not see it. Browser navigations (Accept:
// text/html) get the read-only detail page (T0210); every other client
// gets the JSON contract.
//
// The default JSON body is the single object + its selected version, the
// shape every existing consumer reads. A browse surface (WEB-05) also
// needs the version log and the relations to offer version selection and
// related navigation, so the SAME route answers the detail shape when the
// caller asks for it — `?include=versions,relations` — or names a version
// (`?version=N`). Opt-in rather than always-on keeps the cheap read cheap
// and the existing payload byte-compatible.
func (h *handlers) handleGetObject(w http.ResponseWriter, r *http.Request) {
	if wantsHTML(r) {
		h.handleObjectDetailPage(w, r)
		return
	}
	if objectReadWantsDetail(r) {
		h.handleObjectDetailJSON(w, r)
		return
	}
	res, err := h.svc.GetObject(r.Context(), reader(r), r.PathValue("projectId"), r.PathValue("branchId"), r.PathValue("objectId"))
	if err != nil {
		rsgError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, objectPayloadFromResult(res))
}

// objectReadWantsDetail reports whether the caller asked for the detail
// shape (the version log and relations) rather than the single-version
// object payload. Both spellings are accepted: `include` names the extra
// collections, and a `version` parameter necessarily needs the version log
// to say which version is selected.
func objectReadWantsDetail(r *http.Request) bool {
	q := r.URL.Query()
	if q.Get("version") != "" {
		return true
	}
	for _, part := range strings.Split(q.Get("include"), ",") {
		switch strings.TrimSpace(part) {
		case "versions", "relations":
			return true
		}
	}
	return false
}

// objectVersionQuery parses the browse surface's ?version= selector. A
// missing value selects the latest version; a value that is not a positive
// integer is refused on the wire rather than silently treated as latest —
// a reader who asked for a version must never be shown a different one.
func objectVersionQuery(w http.ResponseWriter, r *http.Request) (*int, bool) {
	raw := r.URL.Query().Get("version")
	if raw == "" {
		return nil, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		authhttp.WriteError(w, r, http.StatusBadRequest, rsg.CodeValidation,
			"version must be a positive integer")
		return nil, false
	}
	return &n, true
}

// handleObjectDetailJSON answers the detail shape: the object, every
// version (oldest first), the selected version's content, and the
// relations incident to the object with both endpoints resolved.
func (h *handlers) handleObjectDetailJSON(w http.ResponseWriter, r *http.Request) {
	versionNo, ok := objectVersionQuery(w, r)
	if !ok {
		return
	}
	detail, err := h.svc.GetObjectDetail(r.Context(), reader(r),
		r.PathValue("projectId"), r.PathValue("branchId"), r.PathValue("objectId"), versionNo)
	if err != nil {
		rsgError(w, r, err)
		return
	}
	// The same URL serves the page and the JSON contract; shared caches
	// must key on Accept so the two never cross-serve.
	w.Header().Add("Vary", "Accept")
	authhttp.WriteJSON(w, http.StatusOK, objectDetailPayloadFrom(detail))
}

// objectVersionPayload is one immutable version row of the detail read.
type objectVersionPayload struct {
	VersionID      string           `json:"version_id"`
	VersionNo      int              `json:"version_no"`
	StateID        string           `json:"state_id"`
	Title          string           `json:"title"`
	LifecycleState string           `json:"lifecycle_state"`
	SchemaRef      schemaRefPayload `json:"schema_ref"`
	Payload        json.RawMessage  `json:"payload"`
	CreatedBy      string           `json:"created_by"`
	CreatedAt      string           `json:"created_at"`
	// Current marks the object's newest version (the log's head).
	Current bool `json:"current"`
}

// objectRelationPayload is one relation incident to the object, with the
// other endpoint's identity resolved (its drill-down coordinate, unlike
// the write payload's raw version ids).
type objectRelationPayload struct {
	RelationType string `json:"relation_type"`
	// Direction is "outgoing" when the selected object is the relation's
	// source and "incoming" when it is the target.
	Direction       string `json:"direction"`
	OtherObjectID   string `json:"other_object_id"`
	OtherObjectType string `json:"other_object_type"`
	OtherTitle      string `json:"other_title"`
	VersionNo       int    `json:"version_no"`
	CreatedBy       string `json:"created_by"`
	CreatedAt       string `json:"created_at"`
}

// objectDetailPayload is the browse surface's read: the object payload for
// the SELECTED version, the whole version log, and the relations.
type objectDetailPayload struct {
	objectPayload
	ProjectSlug       string                  `json:"project_slug"`
	ProjectName       string                  `json:"project_name"`
	BranchID          string                  `json:"branch_id"`
	BranchName        string                  `json:"branch_name"`
	SelectedVersionNo int                     `json:"selected_version_no"`
	Versions          []objectVersionPayload  `json:"versions"`
	Relations         []objectRelationPayload `json:"relations"`
}

// objectDetailPayloadFrom renders the service read as the wire contract.
// Relations are classified by which endpoint's version belongs to THIS
// object; the other endpoint's coordinate is its container object id (the
// browse surface resolves its branch from the outline, exactly as the
// server-rendered page does — no coordinate is fabricated here).
func objectDetailPayloadFrom(d rsg.ObjectDetail) objectDetailPayload {
	p := objectDetailPayload{
		objectPayload: objectPayload{
			ID:             d.Object.ID,
			ProjectID:      d.Object.ProjectID,
			ObjectType:     d.Object.ObjectType,
			CurrentVersion: d.Object.CurrentVersionNo,
			VersionID:      d.Selected.ID,
			VersionNo:      d.Selected.VersionNo,
			StateID:        d.Selected.StateID,
			Title:          d.Selected.Title,
			LifecycleState: string(d.Selected.LifecycleState),
			SchemaRef:      schemaRefPayload{ID: d.Selected.SchemaID, Version: d.Selected.SchemaVersion},
			Payload:        d.Selected.Payload,
		},
		ProjectSlug:       d.Project.Slug,
		ProjectName:       d.Project.Name,
		BranchID:          d.Branch.ID,
		BranchName:        d.Branch.Name,
		SelectedVersionNo: d.Selected.VersionNo,
		Versions:          make([]objectVersionPayload, 0, len(d.Versions)),
		Relations:         make([]objectRelationPayload, 0, len(d.Relations)),
	}
	for _, v := range d.Versions {
		p.Versions = append(p.Versions, objectVersionPayload{
			VersionID:      v.ID,
			VersionNo:      v.VersionNo,
			StateID:        v.StateID,
			Title:          v.Title,
			LifecycleState: string(v.LifecycleState),
			SchemaRef:      schemaRefPayload{ID: v.SchemaID, Version: v.SchemaVersion},
			Payload:        v.Payload,
			CreatedBy:      v.CreatedBy,
			CreatedAt:      v.CreatedAt.UTC().Format(time.RFC3339),
			Current:        v.VersionNo == d.Object.CurrentVersionNo,
		})
	}
	versionIDs := make(map[string]struct{}, len(d.Versions))
	for _, v := range d.Versions {
		versionIDs[v.ID] = struct{}{}
	}
	for _, rv := range d.Relations {
		_, isSource := versionIDs[rv.Relation.SourceObjectVersionID]
		other := rv.Target
		direction := "outgoing"
		if !isSource {
			other = rv.Source
			direction = "incoming"
		}
		p.Relations = append(p.Relations, objectRelationPayload{
			RelationType:    rv.Relation.RelationType,
			Direction:       direction,
			OtherObjectID:   other.ObjectID,
			OtherObjectType: other.ObjectType,
			OtherTitle:      other.Title,
			VersionNo:       rv.Relation.VersionNo,
			CreatedBy:       rv.Relation.CreatedBy,
			CreatedAt:       rv.Relation.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return p
}

// queryPayload is the client-visible RSG graph slice (T0209): the resolved
// project + slice state (null = project-wide) plus the node and edge lists.
type queryPayload struct {
	ProjectID string            `json:"project_id"`
	StateID   *string           `json:"state_id"`
	Objects   []objectPayload   `json:"objects"`
	Relations []relationPayload `json:"relations"`
}

// handleQuery: GET /api/v1/projects/{projectId}/query — the RSG graph
// query (T0209). Query string filters, all optional and repeatable where
// they are lists:
//
//	object_type=material&object_type=finding  seed objects by type
//	relation_type=derived_from               seed relations by type
//	state_id=… | branch_id=…                 pin the slice (mutually exclusive)
//	depth=0..5                               recursive relation traversal depth
//
// The read is exactly as visible as its project: an invisible project
// answers 404 project not found (existence hiding), never 403.
func (h *handlers) handleQuery(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	input := rsg.QueryInput{
		ObjectTypes:   q["object_type"],
		RelationTypes: q["relation_type"],
		StateID:       q.Get("state_id"),
		BranchID:      q.Get("branch_id"),
	}
	if raw := q.Get("depth"); raw != "" {
		depth, err := strconv.Atoi(raw)
		if err != nil {
			authhttp.WriteError(w, r, http.StatusBadRequest, rsg.CodeValidation,
				"depth must be an integer between 0 and "+strconv.Itoa(rsg.MaxQueryDepth))
			return
		}
		input.Depth = depth
	}
	res, err := h.svc.Query(r.Context(), reader(r), r.PathValue("projectId"), input)
	if err != nil {
		rsgError(w, r, err)
		return
	}
	payload := queryPayload{
		ProjectID: res.ProjectID,
		Objects:   make([]objectPayload, 0, len(res.Objects)),
		Relations: make([]relationPayload, 0, len(res.Relations)),
	}
	if res.StateID != "" {
		stateID := res.StateID
		payload.StateID = &stateID
	}
	for _, obj := range res.Objects {
		payload.Objects = append(payload.Objects, objectPayloadFromResult(obj))
	}
	for _, rel := range res.Relations {
		payload.Relations = append(payload.Relations, relationPayloadFromResult(rel))
	}
	authhttp.WriteJSON(w, http.StatusOK, payload)
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
	status, code, message := rsgErrorOutcome(err)
	authhttp.WriteError(w, r, status, code, message)
}

// rsgErrorOutcome is rsgError's mapping split off so the HTML page can
// answer the exact same outcome the JSON envelope answers.
func rsgErrorOutcome(err error) (status int, code, message string) {
	switch {
	case errors.Is(err, projects.ErrProjectNotFound):
		return http.StatusNotFound, projects.CodeProjectNotFound, "project not found"
	case errors.Is(err, rsg.ErrForbidden):
		return http.StatusForbidden, rsg.CodeForbidden, "you are not permitted to write scientific state in this project"
	case errors.Is(err, branches.ErrBranchNotFound) || errors.Is(err, states.ErrBranchNotFound):
		return http.StatusNotFound, states.CodeBranchNotFound, "branch not found"
	case errors.Is(err, branches.ErrBranchNameTaken):
		return http.StatusConflict, branches.CodeBranchNameTaken, "branch name already taken"
	case errors.Is(err, branches.ErrPublicBranchInPrivateProject):
		return http.StatusForbidden, branches.CodePublicBranchInPrivateProject, "a private project cannot host a public branch"
	case errors.Is(err, branches.ErrBaseStateNotFound):
		return http.StatusNotFound, branches.CodeBaseStateNotFound, "base state not found"
	case errors.Is(err, states.ErrStateNotFound) || errors.Is(err, branches.ErrStateNotFound):
		return http.StatusNotFound, states.CodeStateNotFound, "state not found"
	case errors.Is(err, states.ErrStateExists):
		return http.StatusConflict, states.CodeStateExists, "state already exists"
	case errors.As(err, new(*states.StateConflictError)):
		return http.StatusConflict, states.CodeBranchStateConflict, "branch state conflict — re-read the branch head and retry"
	case errors.As(err, new(*states.BranchNotActiveError)) || errors.As(err, new(*branches.NotActiveError)) || errors.Is(err, branches.ErrBranchNotActive):
		return http.StatusConflict, states.CodeBranchNotActive, "branch lifecycle is not active"
	case errors.As(err, new(*states.MainFrozenDirectWriteError)):
		// T0601: main is frozen, so the ONLY way to move it is the Research
		// PR merge docs/09 §3 leaves open. A direct semantic write is
		// refused here with the one code docs/45 gives this outcome, and
		// the caller can tell that it was the freeze that blocked it (not a
		// permission, a conflict or a store failure) from the code alone.
		return http.StatusForbidden, states.CodeMainFrozenDirectWrite, err.Error()
	case errors.Is(err, sciobjects.ErrObjectNotFound):
		return http.StatusNotFound, sciobjects.CodeObjectNotFound, "object not found"
	case errors.Is(err, sciobjects.ErrVersionNotFound):
		return http.StatusNotFound, sciobjects.CodeVersionNotFound, "object version not found"
	case errors.As(err, new(*sciobjects.VersionConflictError)):
		return http.StatusConflict, sciobjects.CodeVersionConflict, "expected_version mismatch — re-read the object and retry"
	case errors.Is(err, relations.ErrRelationNotFound):
		return http.StatusNotFound, relations.CodeRelationNotFound, "relation not found"
	case errors.Is(err, relations.ErrRelationVersionNotFound):
		return http.StatusNotFound, relations.CodeRelationVersionNotFound, "relation version not found"
	case errors.As(err, new(*relations.ReferencedVersionNotFoundError)) || errors.Is(err, relations.ErrReferencedVersionNotFound):
		return http.StatusNotFound, relations.CodeObjectVersionNotFound, "referenced object version not found"
	case errors.As(err, new(*relations.UnknownRelationTypeError)):
		return http.StatusBadRequest, relations.CodeRSGValidationFailed, "relation type is not in the V1 catalog"
	case errors.As(err, new(*rsgvalidation.GateBlockedError)):
		var blocked *rsgvalidation.GateBlockedError
		errors.As(err, &blocked)
		return http.StatusUnprocessableEntity, blocked.Code(), "the write was refused: the commit's validation gate is blocked (run :validate for the full report)"
	case errors.As(err, new(*rsg.EvidenceRefUnavailableError)):
		// One outcome for every reason a version cannot be pinned at that end
		// of an evidence assertion (does not exist / carries no publication /
		// is published to its own members only / belongs to another project).
		// The message is the error's own and says none of that: a caller must
		// not learn from the wire which of the reasons applied (docs/45).
		return http.StatusNotFound, rsg.CodeEvidenceRefUnavailable, err.Error()
	case errors.As(err, new(*rsg.LiteratureEvidenceUnitUnnamedError)):
		// A literature assertion with no evidence unit is the caller's OWN
		// permanent input mistake (docs/10 §6) — resending it changes nothing
		// — so it answers a 400 and never the retryable 503 (docs/45). The
		// code is the semantic check's own, and the message is its advisory,
		// so the author is told which unit to locate rather than merely that
		// the write was refused.
		var unnamed *rsg.LiteratureEvidenceUnitUnnamedError
		errors.As(err, &unnamed)
		return http.StatusBadRequest, unnamed.Code(), unnamed.Error()
	case errors.Is(err, rsg.ErrValidation):
		return http.StatusBadRequest, rsg.CodeValidation, err.Error()
	default:
		return http.StatusServiceUnavailable, rsg.CodeUnavailable, "service unavailable"
	}
}
