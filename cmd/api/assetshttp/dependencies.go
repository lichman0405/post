package assetshttp

import (
	"context"
	"net/http"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/observability"
)

// The project-side dependency read (T0707): which fixed asset versions one
// project depends on, and how.
//
// # Where the route comes from (and where it does not)
//
// GET /api/v1/projects/{projectId}/dependencies is NOT in the contract, and
// it is mounted on the v1 mux anyway — the way cmd/api/provenancehttp mounts
// its projection walk (T0505) and the way this package's own browse list is
// mounted (T0709): "provenance" appears zero times in specs/api/openapi.yaml
// and that read is a product surface all the same. The contract has no path
// for a project's dependency list, and adding one is a product decision that
// does not belong to an implementation task — the same note page.go carries
// for GET /api/v1/assets. Nothing about the contract's own paths is affected:
// the patterns are distinct and the more specific one wins where they are not.
//
// # The order of the steps is the order of what may be disclosed
//
// The project read gate runs FIRST, over the same *projects.Service every
// project read runs (T0106): a project this caller may not read answers the
// existence-hiding 404 the project surface answers, so the route cannot be
// used to learn that a private project exists — or that it has dependencies.
// Only then is the membership bit resolved and the rows read. The rows come
// back RAW (private usages included) and assets.BuildProjectDependencies
// decides what may be rendered, for the reason its file comment gives; this
// file serializes the model's answer and filters nothing, because a handler
// that dropped a field on its way out would be a second implementation of the
// disclosure rule, in the layer that has no test for it.
//
// A non-member of a PUBLIC project reaches this route (a public project is
// readable by anyone, docs/12 §2) and the model answers them the project's
// public declarations only. An anonymous caller is a non-member for that
// purpose, exactly as the membership read resolves it (no session, no
// membership to read).

// CodeAssetDependenciesUnavailable: the dependency read failed, so no honest
// list can be built. A list built over a failed read would report "this
// project depends on nothing" for a repository nobody finished looking at —
// and that report is a claim about the project, not a gap.
const CodeAssetDependenciesUnavailable = "ASSET_DEPENDENCIES_UNAVAILABLE"

// DependencyReader resolves one project's recorded dependencies. The
// production value is *persistence.ProjectDependencyStore.
//
// The rows are returned as stored: filtering is the model's
// (assets.BuildProjectDependencies).
type DependencyReader interface {
	ListProjectDependencies(ctx context.Context, projectID string) ([]assets.ProjectDependencyState, error)
}

// handleProjectDependencies serves GET
// /api/v1/projects/{projectId}/dependencies.
func (h *handlers) handleProjectDependencies(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectId")

	// The project read gate: the same service, the same existence-hiding
	// answer, as every other project read. A refusal here ends the request
	// before any row is read.
	if _, err := h.projects.Get(r.Context(), reader(r), projectID); err != nil {
		writeProjectGateError(w, r, err)
		return
	}

	// The membership bit: the caller's standing in THIS project, resolved
	// through the same read the asset page uses (viewer, page.go) — one
	// implementation of "is this caller a member", not a second one here. A
	// store failure is answered rather than silently downgraded to
	// "not a member", which would be a list that is wrong in the direction
	// nobody would report: a member would be shown the network's view of
	// their own project.
	viewer, err := h.viewer(r, projectID)
	if err != nil {
		observability.LoggerFromContext(r.Context()).Error("project dependencies: membership read failed", "error", err, "project_id", projectID)
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, CodeAssetDependenciesUnavailable,
			"dependency data is temporarily unavailable")
		return
	}

	rows, err := h.dependencies.ListProjectDependencies(r.Context(), projectID)
	if err != nil {
		observability.LoggerFromContext(r.Context()).Error("project dependencies: read failed", "error", err, "project_id", projectID)
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, CodeAssetDependenciesUnavailable,
			"dependency data is temporarily unavailable")
		return
	}
	// The viewer value is converted rather than rebuilt field by field: the
	// two types are the same one bit of information (is this caller inside
	// the project), and a conversion makes it impossible for the two copies
	// to drift apart (staticcheck S1016 says the same thing).
	authhttp.WriteJSON(w, http.StatusOK,
		assets.BuildProjectDependencies(rows, assets.ProjectDependencyViewer(viewer)))
}
