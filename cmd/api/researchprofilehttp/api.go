// Package researchprofilehttp is the transport for the Research Profile and
// the Organization Profile (T0808): the two public reads docs/05 §4 lists as
// network entity pages and docs/42 names ("Research Profile").
//
// The package carries no policy. Every rule about WHAT may be rendered lives
// in internal/application/researchprofile, where it is unit-tested against
// the document that states it; here a service answer becomes a status code
// and the model's own JSON tags become the body (docs/52: the transport
// translates, the application orchestrates).
//
// Both routes are reads and both are public: GET flows through the guard the
// same way the profile read does, and neither takes an actor — the answer is
// the same for an anonymous caller as for the owner, which is what makes it
// safe to serve without a session.
package researchprofilehttp

import (
	"net/http"

	"github.com/lichman0405/post/internal/application/researchprofile"
)

// Deps carries the adapters (ports, docs/52) this API needs.
type Deps struct {
	Reader researchprofile.Reader
}

// API is the mounted research-profile surface.
type API struct {
	handlers *handlers
}

// New wires the service.
func New(deps Deps) *API {
	return &API{handlers: &handlers{svc: researchprofile.NewService(deps.Reader)}}
}

// Service exposes the wired service so a sibling surface can share the exact
// same instance rather than build a second one over the same rows.
func (a *API) Service() *researchprofile.Service { return a.handlers.svc }

// Register mounts both routes on the shared guarded /api/v1 mux (the guard is
// authhttp.API.Guard, composed in cmd/api/main.go).
//
// The two paths are deliberately different in shape, and each mirrors the
// identity its own surface is addressed by:
//
//   - /api/v1/users/{userID}/research-profile — id-keyed, like the profile
//     read it extends: a user's URL must not change when they rename their
//     handle (docs/21 §2, stable ids are the reference identity).
//   - /api/v1/organizations/{slug}/profile — slug-keyed, because the slug IS
//     the organization's stable public identity and is not mutable
//     (internal/application/orgs). The organization surface's own management
//     routes stay id-keyed; this one is the public page.
//
// Neither pattern conflicts with what is already registered: the user route
// is one segment longer than the profile read's, and the organization route
// is more specific than the /api/v1/organizations/ subtree mount, so
// ServeMux resolves it to this package (a request with three segments cannot
// match the two-segment {orgId} pattern, and the two sets are disjoint).
func (a *API) Register(mux *http.ServeMux) {
	h := a.handlers
	mux.HandleFunc("GET /api/v1/users/{userID}/research-profile", h.handleGetPersonProfile)
	mux.HandleFunc("GET /api/v1/organizations/{slug}/profile", h.handleGetOrganizationProfile)
}
