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

// Routes registers the auth surface and returns the guarded subtree
// handler. New product routes (T0103+) register on this mux and inherit
// the write guard automatically.
func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	h := a.handlers
	mux.HandleFunc("POST /api/v1/auth/signup", h.handleSignup)
	mux.HandleFunc("POST /api/v1/auth/login", h.handleLogin)
	mux.HandleFunc("POST /api/v1/auth/logout", h.handleLogout)
	mux.HandleFunc("GET /api/v1/auth/session", h.handleSession)
	mux.HandleFunc("GET /api/v1/auth/oidc/authorize-url", h.handleOIDCAuthorize)
	mux.HandleFunc("GET /api/v1/auth/oidc/callback", h.handleOIDCCallback)
	return a.guard.guard(mux)
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
