package provenancehttp

import (
	"context"
	"net/http"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/provenance"
)

// Store is the projection read port: the provenance edge set of one
// project, and the lineage start node of one object. The production
// implementation is *ProjectionStore in this package (T0505's scope
// excludes internal/persistence, so the adapter travels with the
// transport).
type Store interface {
	// ListEdges returns every projected provenance edge of the project
	// (migration 00043), oldest first. An empty project answers an empty
	// list, never an error.
	ListEdges(ctx context.Context, projectID string) ([]provenance.Edge, error)
	// ObjectStart resolves the lineage start: versionNo nil selects the
	// object's latest version, a number pins one exact version. The path
	// project is enforced inside the store — the object is checked to
	// belong to projectID BEFORE any version lookup, so a foreign object
	// and a nonexistent object answer the same ErrObjectNotFound whatever
	// versionNo says (docs/45: no version-count oracle). It fails with
	// sciobjects.ErrObjectNotFound / ErrVersionNotFound.
	ObjectStart(ctx context.Context, objectID, projectID string, versionNo *int) (ObjectStart, error)
}

// ObjectStart is one resolved lineage start: the pinned version node.
// Project membership is checked inside the store, not returned — the
// caller passed the path project in.
type ObjectStart struct {
	Node provenance.Node
}

// Gate is the project read gate — the same member-only, existence-hiding
// rule as the project surface (T0106). The production implementation is
// *projects.Service (projectAPI.Service() in main.go).
type Gate interface {
	Get(ctx context.Context, r projects.Reader, projectID string) (domain.Project, error)
}

// Deps carries the adapters the surface needs.
type Deps struct {
	Store Store
	Gate  Gate
}

// New wires the handlers.
func New(deps Deps) *API {
	return &API{handlers: &handlers{store: deps.Store, gate: deps.Gate}}
}

// API registers the provenance routes on the shared /api/v1 mux.
type API struct {
	handlers *handlers
}

// Register mounts the read-only provenance surface directly on the v1 mux
// (the full-path patterns are more specific than the projects subtree, so
// they coexist with it). No state-changing verb has a route — the
// projection is written by the database trigger on relation_versions, never
// over HTTP.
func (a *API) Register(v1 *http.ServeMux) {
	v1.HandleFunc("GET /api/v1/projects/{projectId}/provenance/graph", a.handlers.handleGraph)
	v1.HandleFunc("GET /api/v1/projects/{projectId}/objects/{objectId}/lineage", a.handlers.handleLineage)
}
