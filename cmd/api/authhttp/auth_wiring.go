package authhttp

import (
	"net/http"

	"github.com/lichman0405/post/internal/application/authn"
)

// The auth API wiring: adapters go in, one guarded handler comes out.
// cmd/api builds the production graph (pgx + Redis); the e2e suite builds
// the same graph over miniredis + memory stores, so the tests exercise
// the exact production middleware and handlers — only the storage adapters
// differ.

// Deps carries the adapters (ports, docs/52) the auth API needs.
type Deps struct {
	Users      authn.UserStore
	Sessions   authn.SessionStore
	Limiter    authn.RateLimiter
	OIDCClient authn.OIDCProvider // nil = OIDC disabled
	Cfg        authn.Config
	Secure     bool // prod layer: Secure cookies
}

// New wires the service and its routes.
func New(deps Deps) *API {
	svc := authn.NewService(deps.Users, deps.Sessions, deps.Limiter, deps.OIDCClient, deps.Cfg)
	return &API{
		handlers: &handlers{
			svc:    svc,
			cfg:    deps.Cfg,
			secure: deps.Secure,
			web:    deps.Cfg.WebOrigin,
		},
		guard: guard{
			svc:     svc,
			cfg:     deps.Cfg,
			secure:  deps.Secure,
			webHost: hostOf(deps.Cfg.WebOrigin),
		},
	}
}

// API is the mounted /api/v1/auth subtree.
type API struct {
	handlers *handlers
	guard    guard
}

// Routes registers the auth routes on mux and returns the guarded auth
// subtree handler. Product routes (T0102+ profilehttp) register on the
// same mux via their own Register methods and inherit the write guard
// automatically.
func (a *API) Register(mux *http.ServeMux) {
	h := a.handlers
	mux.HandleFunc("POST /api/v1/auth/signup", h.handleSignup)
	mux.HandleFunc("POST /api/v1/auth/login", h.handleLogin)
	mux.HandleFunc("POST /api/v1/auth/logout", h.handleLogout)
	mux.HandleFunc("GET /api/v1/auth/session", h.handleSession)
	mux.HandleFunc("GET /api/v1/auth/oidc/authorize-url", h.handleOIDCAuthorize)
	mux.HandleFunc("GET /api/v1/auth/oidc/callback", h.handleOIDCCallback)
}

// Guard wraps a /api/v1 route set with the session/CSRF guard (see guard):
// anonymous writes are 401 before routing, session-bearing writes need the
// CSRF token, reads flow (product reads decide their own visibility).
func (a *API) Guard(next http.Handler) http.Handler {
	return a.guard.guard(next)
}

// Routes returns the guarded auth-only subtree — the pre-product wiring.
// cmd/api composes Register + Guard explicitly instead, so the product
// APIs mount on the same guarded mux (one guard for the whole subtree,
// not one per surface).
func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	a.Register(mux)
	return a.Guard(mux)
}

// hostOf extracts the host part of an origin URL ("http://x:3000" -> "x:3000").
func hostOf(origin string) string {
	// WebOrigin is validated at config load (scheme://host, no path).
	for i := 0; i+3 <= len(origin); i++ {
		if origin[i] == ':' && origin[i+1] == '/' && origin[i+2] == '/' {
			return origin[i+3:]
		}
	}
	return origin
}
