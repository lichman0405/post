package freezehttp

import (
	"net/http"
)

// The freeze API wiring: the T0601 freeze command goes in, one guarded
// route comes out (the guard itself is owned by authhttp and composed in
// cmd/api/main.go).
//
// cmd/api builds the production graph. The E2E suite
// (tests/integration/freeze_main_e2e_test.go) builds the same surface the
// same way: freezehttp.New + Register over real PostgreSQL and a real
// Gitea, behind the same guard, reached by the same contract request — not
// by calling the command directly, and not by registering an equivalent
// route by hand.

// Deps carries the command the freeze route needs.
type Deps struct {
	// Command is the wired freeze command (the production value is
	// *mainfreeze.Command).
	Command freezeCommand
}

// New wires the handlers.
func New(deps Deps) *API {
	return &API{handlers: &handlers{cmd: deps.Command}}
}

// API is the mounted freeze surface.
type API struct {
	handlers *handlers
}

// Register mounts the freeze route on the v1 mux.
//
// The pattern is the contract's literal path: the ":freeze" suffix is a
// literal segment suffix after the {projectId} wildcard, which Go's
// ServeMux accepts (a wildcard must be a whole segment; a literal suffix
// within the same segment is only a problem when the WILDCARD is partial,
// as mergehttp's "{number}:merge" is). No other method is registered on
// this path: there is no unfreeze route to register, and no second way to
// move the flag in either direction.
func (a *API) Register(v1 *http.ServeMux) {
	v1.HandleFunc("POST /api/v1/projects/{projectId}/main:freeze", a.handlers.handleFreeze)
}
