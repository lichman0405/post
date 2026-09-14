// Package conflicthttp is the HTTP surface of the Scientific Conflict
// Resolution UI (T0407): the conflict view read and the resolution plan
// write over the three-way base/source/target triple one semantic merge
// is computed over.
//
//	GET /api/v1/projects/{projectId}/conflicts
//	    ?base_state_id=...&source_state_id=...&target_state_id=...
//	    the conflict report (diff + verdicts, the base/A/B values ride
//	    along in the report's changes), the per-side evidence context of
//	    the conflicted objects, and the decisions already recorded.
//	PUT /api/v1/projects/{projectId}/resolutions
//	    body: the pinned triple + the decisions (target_kind/target_id,
//	    conflict code/fields/payload_keys/paired object, kind, note).
//	    Records the plan (audited, in one transaction) and returns it.
//
// The write is a human decision (docs/60): the handler resolves the
// caller's identity from the session, the service authorizes through the
// project surface + policy engine, and the stored row names the human who
// decided. The agent explanation the report carries is rendered by the
// UI as advisory-only — nothing here derives a decision from it.
//
// Reads run the project read gate first (T0106 read semantics, the same
// existence-hiding 404 as every other project read); writes are guarded
// by the session/CSRF guard like every other v1 surface.
package conflicthttp

import (
	"context"
	"net/http"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/resolutions"
	"github.com/lichman0405/post/internal/domain"
)

// Viewer is the read surface the handlers call: the conflict view (report
// + evidence + recorded decisions). The production implementation is
// *resolutions.Service.
type Viewer interface {
	View(ctx context.Context, projectID, baseStateID, sourceStateID, targetStateID string) (*resolutions.View, error)
}

// Saver is the write surface the handlers call: record one plan
// submission. The production implementation is *resolutions.Service.
type Saver interface {
	Save(ctx context.Context, actor domain.User, in resolutions.SaveInput) ([]domain.ConflictResolution, error)
}

// ProjectReader is the visibility gate the handlers run before any read:
// resolve whether the caller may read the project at all (the production
// implementation is internal/application/projects.Service — Get with a
// Reader evaluates the T0106 read matrix and answers ErrProjectNotFound
// for a denied read, the existence-hiding 404).
type ProjectReader interface {
	Get(ctx context.Context, r projects.Reader, projectID string) (domain.Project, error)
}

// Deps carries the surfaces the conflict API needs.
type Deps struct {
	Viewer   Viewer
	Saver    Saver
	Projects ProjectReader
}

// New wires the handler.
func New(deps Deps) *API {
	return &API{handlers: &handlers{viewer: deps.Viewer, saver: deps.Saver, projects: deps.Projects}}
}

// API is the mounted conflict resolution surface.
type API struct {
	handlers *handlers
}

// Register mounts the conflict routes on the guarded v1 mux.
func (a *API) Register(v1 *http.ServeMux) {
	v1.HandleFunc("GET /api/v1/projects/{projectId}/conflicts", a.handlers.handleGetConflicts)
	v1.HandleFunc("PUT /api/v1/projects/{projectId}/resolutions", a.handlers.handlePutResolutions)
}
