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
	// Publish is the publish use case (T0705). The production value is
	// *assetpublish.Command, over the store that owns the publish
	// transaction. Like State and Projects above it is required: a surface
	// wired without one of its three ports is a wiring bug, not a
	// degradation to answer around.
	Publish PublishCommand
	// Pages is the read-only asset page reader (T0709). The production value
	// is *persistence.AssetPageStore.
	Pages PageReader
	// Members is the membership read the asset page needs to tell a member's
	// view of an asset from the network's (T0709). The production value is
	// the same *projects.Service the Projects gate is.
	Members Membership
	// Dependencies is the project-side dependency read (T0707). The
	// production value is *persistence.ProjectDependencyStore.
	Dependencies DependencyReader
}

// New wires the handlers.
func New(deps Deps) *API {
	return &API{handlers: &handlers{
		state:        deps.State,
		projects:     deps.Projects,
		publish:      deps.Publish,
		pages:        deps.Pages,
		members:      deps.Members,
		dependencies: deps.Dependencies,
	}}
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
// Two routes are registered, and they are the pair docs/23 §4 describes:
// the preview that says who would see what, and the publish that does it.
// The preview has no GET form: its input is a candidate document, which
// does not fit in a query string, and a GET carrying the caller's
// proposed-manifest bytes in a URL would put a version document in access
// logs. The publish is a POST for the same reason plus its own — it is a
// governed write, so it needs the session and CSRF guard the v1 subtree
// applies to every write, and its Idempotency-Key is a header, which is
// where docs/22 puts it.
func (a *API) Register(v1 *http.ServeMux) {
	v1.HandleFunc("POST /api/v1/projects/{projectId}/assets:publish-preview", a.handlers.handlePublishPreview)
	v1.HandleFunc("POST /api/v1/projects/{projectId}/assets:publish", a.handlers.handlePublish)
	// The asset hub's reads (T0709). The page data path is the contract's
	// (specs/api/openapi.yaml: /assets/{assetId}, `security: []`), including
	// the fact that it is not nested under a project: the scope that decides
	// visibility is the ASSET, and the pid is the identity a client holds.
	// The browse list is not in the contract; see page.go for why it is
	// mounted here anyway.
	v1.HandleFunc("GET /api/v1/assets/{assetId}", a.handlers.handleAssetPage)
	v1.HandleFunc("GET /api/v1/assets", a.handlers.handleAssetBrowse)
	// The project-side dependency read (T0707). Not a contract path: see
	// dependencies.go for why it is mounted here anyway. The pattern is
	// under the projects subtree the project surface owns, and it is more
	// specific than that subtree's routes, so the two coexist.
	v1.HandleFunc("GET /api/v1/projects/{projectId}/dependencies", a.handlers.handleProjectDependencies)
}
