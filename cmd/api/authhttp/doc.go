// Package authhttp is the authentication transport layer of the POST API
// (docs/52: transport/http -> application -> domain): the /api/v1 guard and
// the auth routes (signup, login, logout, session, OIDC). It is importable
// so the e2e suite (tests/e2e) drives the exact production middleware and
// handlers over real HTTP — only the storage adapters differ.
//
// Policy in one line: every state-changing request under /api/v1 requires a
// valid session + CSRF token, except the pre-auth login/signup routes which
// get Origin + JSON-content-type checks instead; reads are never blocked
// here. Future product routes register on the same subtree and inherit the
// guard.
package authhttp
