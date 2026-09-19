package evidencehttp

import (
	"context"
	"net/http"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/evidence"
)

// Service is the evidence-graph read port the handlers call. The production
// implementation is *evidencegraph.Service.
type Service interface {
	// ObjectEvidence reads one object's evidence grouped by the target
	// version each assertion pins; versionNo nil reads the whole object,
	// a number pins one version. It fails with sciobjects.ErrObjectNotFound
	// for an object outside the path project (the same answer a nonexistent
	// one gets) and sciobjects.ErrVersionNotFound for a version the log does
	// not hold.
	ObjectEvidence(ctx context.Context, projectID, objectID string, versionNo *int) (evidence.ObjectEvidence, error)
	// HypothesisEvidence reads the two-section hypothesis page. An object
	// that is not a hypothesis answers sciobjects.ErrObjectNotFound.
	HypothesisEvidence(ctx context.Context, projectID, objectID string) (evidence.HypothesisEvidence, error)
}

// Gate is the project read gate — the same member-only, existence-hiding
// rule as every other project read (T0106). The production implementation is
// *projects.Service (projectAPI.Service() in main.go).
type Gate interface {
	Get(ctx context.Context, r projects.Reader, projectID string) (domain.Project, error)
}

// Deps carries the adapters the surface needs.
type Deps struct {
	Service Service
	Gate    Gate
}

// New wires the handlers.
func New(deps Deps) *API {
	return &API{handlers: &handlers{service: deps.Service, gate: deps.Gate}}
}

// API registers the evidence-graph routes on the shared /api/v1 mux.
type API struct {
	handlers *handlers
}

// Register mounts the read-only evidence-graph surface directly on the v1
// mux (the full-path patterns are more specific than the projects subtree, so
// they coexist with it).
//
// Both routes are GETs under the project subtree and neither sits under the
// provenance surface's prefix: the evidence graph and the provenance graph
// are different graphs over different tables with different relation
// semantics (CLAUDE.md §9 invariant 10, docs/10 §1), and a route named
// /provenance/... would be the one place a client could reasonably conclude
// otherwise. There is no write verb here — evidence assertions are written by
// the RSG evidence-assertion route inside a state commit, never by this
// surface.
func (a *API) Register(v1 *http.ServeMux) {
	v1.HandleFunc("GET /api/v1/projects/{projectId}/objects/{objectId}/evidence", a.handlers.handleObjectEvidence)
	v1.HandleFunc("GET /api/v1/projects/{projectId}/hypotheses/{objectId}/evidence", a.handlers.handleHypothesisEvidence)
}
