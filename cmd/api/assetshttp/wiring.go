package assetshttp

import (
	"context"
	"net/http"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/domain"
)

// The publication impact preview wiring: the state reader and the project
// read gate go in, one route comes out (the session/CSRF guard is owned by
// authhttp and composed in cmd/api/main.go, like every other product
// route).

// StateReader resolves the current state a preview is computed against.
//
// It takes the CANDIDATE rather than the preview request: what has to be
// resolved are the things the candidate names (its pid, its pins, its
// origin refs, the blob ids its manifest declares), and it answers with
// assets.CurrentState — one entry per canonical ref, one per pin that
// exists, one per blob that exists. See the package doc for which table
// answers for which kind of name, and for the contract that a canonical
// ref the state does not answer for is an error rather than a finding.
type StateReader interface {
	ResolvePreviewState(ctx context.Context, candidate assets.PublishCandidate) (assets.CurrentState, error)
}

// Gate is the project read gate — the same member-only, existence-hiding
// rule as the project surface (T0106). The production implementation is
// *projects.Service (projectAPI.Service() in main.go).
type Gate interface {
	Get(ctx context.Context, r projects.Reader, projectID string) (domain.Project, error)
}

// Deps carries the adapters the surface needs.
type Deps struct {
	// State is the read-only state reader. The production value is
	// *PostgresStateStore (state.go).
	State StateReader
	// Projects is the project read gate.
	Projects Gate
}

// New wires the handlers.
func New(deps Deps) *API {
	return &API{handlers: &handlers{state: deps.State, projects: deps.Projects}}
}

// API is the mounted preview surface.
type API struct {
	handlers *handlers
}

// Register mounts the preview route directly on the v1 mux.
//
// The full-path pattern is more specific than the /api/v1/projects/
// subtree, so it coexists with it (the same technique policyhttp and
// releasehttp use). The path — including the ":publish-preview" suffix in
// one segment — is the contract's (specs/api/openapi.yaml), copied
// verbatim; a client cannot be asked to spell it differently.
//
// There is exactly one route, and it is the only one this package
// registers. The preview has no GET form: its input is a candidate
// document, which does not fit in a query string, and a GET carrying the
// caller's proposed-manifest bytes in a URL would put a version document
// in access logs. It has no sibling for the publish itself: that is
// POST .../assets:publish, T0705's route, which this package does not
// implement.
func (a *API) Register(v1 *http.ServeMux) {
	v1.HandleFunc("POST /api/v1/projects/{projectId}/assets:publish-preview", a.handlers.handlePublishPreview)
}
