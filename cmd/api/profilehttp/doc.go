// Package profilehttp is the HTTP surface of the research profile
// (T0102): reading the public profile and updating the fields the owner
// may edit. Every handler is a thin translation — parse the request, call
// the Service, render JSON (docs/52: transport never contains policy).
//
// The routes register on the guarded /api/v1 mux (authhttp.Register) so
// they inherit the structural session/CSRF guard: anonymous GETs read the
// public profile, PATCH requires a valid session + CSRF token, and the
// owner-only rule is enforced by the application service
// (internal/application/profile).
package profilehttp
