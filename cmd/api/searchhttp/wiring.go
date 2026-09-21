package searchhttp

import (
	"context"
	"net/http"

	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/search"
	"github.com/lichman0405/post/internal/search/answer"
	"github.com/lichman0405/post/internal/search/planner"
	"github.com/lichman0405/post/internal/search/ranking"
	"github.com/lichman0405/post/internal/search/retrieval"
)

// The pipeline's four steps as this surface uses them. Each port is exactly
// the method the pipeline calls, and each one is satisfied structurally by
// the production value named in its comment — so cmd/api passes the objects
// it already builds and this package never constructs one. The alternative
// (the port holding the concrete type) would make every test of this surface
// need a database, and the pipeline's interesting behaviour is not in the
// database.
type (
	// Planner turns a question into a plan. A nil Planner means no planner is
	// configured: the search runs unplanned and says so in the record (the
	// plan column is null).
	// The production value is *planner.Planner.
	Planner interface {
		Plan(ctx context.Context, scope search.Scope, req planner.Request) (planner.Plan, error)
	}
	// Retriever recalls candidates. Required.
	// The production value is *retrieval.Retriever.
	Retriever interface {
		Retrieve(ctx context.Context, scope search.Scope, req retrieval.Request) (retrieval.Result, error)
	}
	// Ranker orders the candidates. Required.
	// The production value is *ranking.Ranker.
	Ranker interface {
		Rank(ctx context.Context, scope search.Scope, req retrieval.Request, res retrieval.Result) (ranking.Result, error)
	}
	// Answerer writes the answer. Required; a nil PROVIDER inside it is the
	// supported "no answer model" state (answer.Deps.Provider).
	// The production value is *answer.Generator.
	Answerer interface {
		Answer(ctx context.Context, in answer.Input) (answer.Answer, error)
	}
	// RecordWriter saves the search. Required — see doc.go's second promise.
	// The production value is *persistence.SearchRecordStore.
	RecordWriter interface {
		Save(ctx context.Context, rec persistence.SearchRecord) (string, error)
	}
)

// Deps carries what the surface needs.
//
// Scope, Retriever, Ranker, Answerer and Records are required; New refuses
// without them, because a search missing any one of them is not a degraded
// search — it is a route that would answer something other than what it
// claims. Planner and Answer's provider are the two OPTIONAL halves of the
// pipeline, and their absence is a state the platform is allowed to be in
// (see doc.go).
type Deps struct {
	// Scope is the scope reader, which is search.ProjectScopeReader rather
	// than a port declared here: it is the SAME port internal/search/scope.go
	// hands to search.ResolveScope, and re-declaring it would be a second
	// definition of "what a scope is read from" — the interface one store
	// satisfies today and two would satisfy differently tomorrow.
	// The production value is *persistence.ProjectStore.
	Scope     search.ProjectScopeReader
	Planner   Planner
	Retriever Retriever
	Ranker    Ranker
	Answerer  Answerer
	Records   RecordWriter
	// Limits bounds the retrieval; the zero value takes the retrieval's own
	// defaults. It is a field rather than a constant so the API can be run
	// with a smaller fan-out (a test, a constrained deployment) without a
	// second code path.
	Limits retrieval.Limits
}

// New wires the handlers.
func New(deps Deps) *API {
	return &API{svc: newService(deps)}
}

// API is the mounted search surface.
type API struct {
	svc *service
}

// Register mounts the search route on the shared /api/v1 mux.
//
// One pattern, one method. The path is the contract's (specs/api/openapi.yaml,
// /search), the mux is the api group's, and the route is registered BEFORE the
// auth guard in main.go like every other product route — the guard is what
// authenticates the request, and a handler behind it can rely on a principal
// being present (nothing here resolves a session itself).
func (a *API) Register(v1 *http.ServeMux) {
	v1.HandleFunc("POST /api/v1/search", a.svc.handleSearch)
}
