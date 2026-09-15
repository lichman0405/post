package mergehttp

import (
	"net/http"
)

// The merge API wiring: the T0406 merge command and the project read gate go
// in, one guarded route comes out (the guard itself is owned by authhttp and
// composed in cmd/api/main.go).
//
// cmd/api builds the production graph. The E2E suite
// (tests/integration/merge_governance_e2e_test.go) builds the same merge
// surface the same way: mergehttp.New + Register over the same real
// PostgreSQL and the same real Gitea, behind the same guard, reached by the
// same contract request — not by calling the merge command directly, and not
// by registering an equivalent route by hand. The one difference is the two
// adapters the suite substitutes: its session store and rate limiter are the
// in-memory implementations, for the reason
// tests/integration/release_e2e_test.go records for its own graph (Redis
// session semantics are orthogonal to this route and covered elsewhere).

// Deps carries the command and the read gate the merge route needs.
type Deps struct {
	// Command is the wired semantic merge service (mergeCommand; the
	// production value is *merge.Service).
	Command mergeCommand
	// Projects is the project read gate: a merge is exactly as reachable as
	// its project. The production adapter is projectAPI.Service().
	Projects projectReader
}

// New wires the handlers.
func New(deps Deps) *API {
	return &API{handlers: &handlers{cmd: deps.Command, projects: deps.Projects}}
}

// API is the mounted merge surface.
type API struct {
	handlers *handlers
}

// Register mounts the merge route on the v1 mux.
//
// The pattern is a REMAINDER wildcard, not the contract's literal
// "{number}:merge": Go's ServeMux rejects a wildcard that is not a whole
// segment ("{number}:merge" panics with "bad wildcard segment"), so the last
// segment is captured whole and the ":merge" suffix is split off in the
// handler — the same technique the branch :validate route uses. A POST to any
// other path under this prefix reaches the same handler and is answered 404 by
// the suffix check: no route is widened by the trick.
//
// The more specific POST .../pull-requests/{number}/reviews route registered by
// reviewhttp wins over this pattern (ServeMux picks the most specific match),
// so the remainder wildcard does not swallow the review submission.
func (a *API) Register(v1 *http.ServeMux) {
	v1.HandleFunc("POST /api/v1/projects/{projectId}/pull-requests/{number...}", a.handlers.handleMerge)
}
