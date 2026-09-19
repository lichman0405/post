package aborthttp

import "net/http"

// The abort-proposal API wiring: the T0602 abort command goes in, one
// guarded route comes out (the guard itself is owned by authhttp and
// composed in cmd/api/main.go).
//
// cmd/api builds the production graph. The integration suite
// (tests/integration/abort_e2e_test.go) builds the same surface the same
// way: aborthttp.New + Register over real PostgreSQL, behind the same
// guard, reached by the contract's request — not by calling the command
// directly, and not by registering an equivalent route by hand.

// Deps carries the command the abort route needs.
type Deps struct {
	// Command is the wired abort command (the production value is
	// *aborts.Service).
	Command abortCommand
}

// New wires the handlers.
func New(deps Deps) *API {
	return &API{handlers: &handlers{cmd: deps.Command}}
}

// API is the mounted abort surface.
type API struct {
	handlers *handlers
}

// Register mounts the abort route on the v1 mux.
//
// The pattern is the contract's path with its last segment expressed the
// way Go's ServeMux can express it. `{objectId}:abort-proposal` is a
// PARTIAL wildcard — a wildcard sharing its segment with a literal suffix —
// and ServeMux refuses those; mergehttp's `{number}:merge` is refused for
// the same reason and is spelled `{number...}` there. Here the wildcard is
// `{objectRef...}` (the rest of the path) and the handler splits the suffix
// off, answering 404 for a segment that does not carry it.
//
// Nothing else is registered on a POST under
// /api/v1/projects/{projectId}/objects/, so the rest-of-path wildcard
// swallows no route that exists. The GET routes that DO live under
// /objects/ (provenancehttp's lineage) are a different method and are
// untouched: the mux matches method and path together, and a request
// matches at most one pattern here.
//
// A SIBLING POST added under the same prefix is not a rewrite of this
// pattern and must not be registered as one: ServeMux keeps both and routes
// by specificity, so a new pattern whose wildcard is a whole segment
// (`.../objects/{objectId}/notes`) wins the paths it names while this one
// keeps the rest — verified, not assumed: with both registered,
// `POST .../objects/abc/notes` reaches the sibling and
// `POST .../objects/abc` still reaches this handler. What this handler
// must keep doing, therefore, is answering a request that names no abort:
// a POST to any other path under the prefix lands HERE, and ServeMux's 405
// is reserved for paths that have other methods registered, so a suffix-less
// segment has to be the 404 pathObjectID returns rather than an error the
// mux would have produced.
func (a *API) Register(v1 *http.ServeMux) {
	v1.HandleFunc("POST /api/v1/projects/{projectId}/objects/{objectRef...}", a.handlers.handleAbortProposal)
}
