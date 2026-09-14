package provenancehttp

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/rsg/provenance"
)

// Wire codes (docs/45: stable codes, no dependency detail). The not-found
// outcomes reuse the owning surfaces' codes so a hidden project and a
// hidden object stay indistinguishable from the real thing.
const (
	CodeValidation  = "PROVENANCE_VALIDATION_FAILED"
	CodeUnavailable = "PROVENANCE_UNAVAILABLE"
)

// ErrValidation marks a client-shaped request (bad direction/max_depth).
// It is answered 400; store failures are wrapped separately and answered
// 503.
var ErrValidation = errors.New("provenance validation")

// handlers owns the read-only provenance routes.
type handlers struct {
	store Store
	gate  Gate
}

// reader resolves the caller for the visibility-aware read (T0106): the
// same resolution every other project read runs. Anonymous reads flow —
// the gate decides what an anonymous reader may see.
func reader(r *http.Request) projects.Reader {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		return projects.Reader{}
	}
	return projects.Reader{UserID: p.User.ID, Authenticated: true}
}

// graphPayload is the wire shape of the whole-project graph read: the
// project boundary plus the projected nodes and edges.
type graphPayload struct {
	ProjectID string `json:"project_id"`
	provenance.Graph
}

// handleGraph: GET /api/v1/projects/{projectId}/provenance/graph — the
// graph list/JSON output (acceptance criterion): every provenance edge of
// the project with both endpoint labels, nodes and edges in deterministic
// order. Exactly as visible as the project itself.
func (h *handlers) handleGraph(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectId")
	if _, err := h.gate.Get(r.Context(), reader(r), projectID); err != nil {
		provenanceError(w, r, err)
		return
	}
	edges, err := h.store.ListEdges(r.Context(), projectID)
	if err != nil {
		provenanceError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, graphPayload{
		ProjectID: projectID,
		Graph:     provenance.Build(edges),
	})
}

// lineagePayload is the wire shape of one object's lineage/impact read: the
// start facts plus the walked subgraph.
type lineagePayload struct {
	ObjectID  string `json:"object_id"`
	VersionID string `json:"version_id"`
	Direction string `json:"direction"`
	MaxDepth  *int   `json:"max_depth,omitempty"`
	provenance.Graph
}

// handleLineage: GET
// /api/v1/projects/{projectId}/objects/{objectId}/lineage — the lineage
// view (acceptance criterion: "可回答 Dataset 从哪来"). direction=upstream
// (default) walks the origins; direction=downstream walks the dependents
// (the impact walk T1007's impact analysis hooks into). version_no pins one
// exact version (default: the object's latest); max_depth bounds the walk
// in edge hops (default: no limit — the walk is cycle-safe and bounded by
// the graph).
//
// The read is as visible as the project in the path, and the gate runs
// FIRST — before the object is even resolved — so a denied reader (or an
// unknown project) answers the existence-hiding PROJECT_NOT_FOUND for every
// object id and every parameter value: whether the object or its version
// exists stays indistinguishable from the project not existing (docs/45,
// the same gate-first order every other read surface uses). Once the gate
// passes, the store resolves the object, checks it belongs to the path
// project BEFORE any version lookup (a foreign object under any project
// prefix answers the object 404 — the same outcome a nonexistent object
// gets — for every version_no, so the version-pinned axis leaks nothing
// either), and the walk runs.
func (h *handlers) handleLineage(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectId")
	objectID := r.PathValue("objectId")
	if _, err := h.gate.Get(r.Context(), reader(r), projectID); err != nil {
		provenanceError(w, r, err)
		return
	}
	dir, ok := provenance.ParseWalkDirection(r.URL.Query().Get("direction"))
	if !ok {
		authhttp.WriteError(w, r, http.StatusBadRequest, CodeValidation,
			"direction must be \"upstream\" or \"downstream\"")
		return
	}
	maxDepth := provenance.UnlimitedDepth
	if raw := r.URL.Query().Get("max_depth"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			authhttp.WriteError(w, r, http.StatusBadRequest, CodeValidation,
				"max_depth must be a non-negative integer")
			return
		}
		maxDepth = n
	}
	var versionNo *int
	if raw := r.URL.Query().Get("version_no"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			authhttp.WriteError(w, r, http.StatusBadRequest, CodeValidation,
				"version_no must be a positive integer")
			return
		}
		versionNo = &n
	}
	start, err := h.store.ObjectStart(r.Context(), objectID, projectID, versionNo)
	if err != nil {
		provenanceError(w, r, err)
		return
	}
	edges, err := h.store.ListEdges(r.Context(), projectID)
	if err != nil {
		provenanceError(w, r, err)
		return
	}
	graph := provenance.Walk(provenance.Build(edges), []provenance.Node{start.Node}, dir, maxDepth)
	var md *int
	if maxDepth != provenance.UnlimitedDepth {
		md = &maxDepth
	}
	authhttp.WriteJSON(w, http.StatusOK, lineagePayload{
		ObjectID:  objectID,
		VersionID: start.Node.VersionID,
		Direction: dir.String(),
		MaxDepth:  md,
		Graph:     graph,
	})
}

// provenanceError maps read outcomes to the wire. The not-found outcomes
// reuse the owning surfaces' codes: a denied read and a nonexistent
// resource answer the same envelope (existence hiding, docs/45).
func provenanceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, projects.ErrProjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, projects.CodeProjectNotFound, "project not found")
	case errors.Is(err, sciobjects.ErrObjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, sciobjects.CodeObjectNotFound, "object not found")
	case errors.Is(err, sciobjects.ErrVersionNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, sciobjects.CodeVersionNotFound, "object version not found")
	case errors.Is(err, ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, CodeValidation, err.Error())
	default:
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "service unavailable")
	}
}
