package authhttp

import (
	"context"
	"crypto/subtle"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
)

// The /api/v1 guard: default-deny for unauthenticated writes (docs/23 §3),
// CSRF-bound state changes, CORS for the web origin. Every route under
// /api/v1 — current and future (T0103+) — inherits this guard by
// registering on the same subtree, so the "unauthenticated write => 401"
// property is structural, not per-route opt-in.
//
// Policy, in order:
//  1. CORS: answer preflights, allow the configured web origin only.
//  2. Session: resolve the post_session cookie (failures are "no session"
//     for reads; writes fail closed).
//  3. Writes (POST/PUT/PATCH/DELETE):
//     a. exempt auth routes (login/signup/oidc): cross-site Origin check
//     + JSON content type — login CSRF and form-planting are blocked
//     without a session to bind;
//     b. everything else: valid session or 401;
//     c. with a session: X-CSRF-Token must match the session's token or
//     403.
//
// The OIDC callback is a GET that completes a login, so it is NOT exempt
// here; its protection is the state cookie binding inside the handler
// (a cross-site attacker cannot set the HttpOnly state cookie).
type guard struct {
	svc     *authn.Service
	cfg     authn.Config
	secure  bool // Secure cookie flag: prod layer only (http dev works)
	webHost string
}

// principal carries the authenticated actor through the request.
type principal struct {
	User    domain.User
	Session authn.Session
}

type principalKey struct{}

func withPrincipal(ctx context.Context, p principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// principalFrom returns the authenticated actor, if the guard resolved one.
func principalFrom(ctx context.Context) (principal, bool) {
	p, ok := ctx.Value(principalKey{}).(principal)
	return p, ok
}

const (
	cookieSession  = "post_session"
	cookieOIDC     = "post_oidc_state"
	headerCSRF     = "X-CSRF-Token"
	headerOrigin   = "Origin"
	headerCT       = "Content-Type"
	headerVary     = "Vary"
	headerACOrigin = "Access-Control-Allow-Origin"
	headerACCred   = "Access-Control-Allow-Credentials"
	headerACMethod = "Access-Control-Allow-Methods"
	headerACHeader = "Access-Control-Allow-Headers"
)

// stateChangingMethods are the verbs the write guard covers. Reads stay
// open (resource-level visibility arrives with the project tasks).
var stateChangingMethods = map[string]bool{
	http.MethodPost:   true,
	http.MethodPut:    true,
	http.MethodPatch:  true,
	http.MethodDelete: true,
}

// exemptWrites are the write routes that exist before authentication and
// therefore cannot hold a session-bound CSRF token: they get the Origin +
// content-type defense instead.
var exemptWrites = map[string]bool{
	"POST /api/v1/auth/signup": true,
	"POST /api/v1/auth/login":  true,
}

func (g *guard) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.applyCORS(w, r) {
			return // preflight answered
		}

		// Resolve the session for every request under the subtree.
		ctx := r.Context()
		if cookie, err := r.Cookie(cookieSession); err == nil && cookie.Value != "" {
			user, sess, err := g.svc.Authenticate(ctx, cookie.Value)
			switch {
			case err == nil:
				ctx = withPrincipal(ctx, principal{User: user, Session: sess})
			case errors.Is(err, authn.ErrSessionNotFound):
				// No/expired session: reads continue, writes answer 401.
			default:
				// The session store itself failed: state is unknowable, so
				// writes fail closed (a read keeps flowing unauthenticated
				// rather than answering 500s for the whole site).
				log := observability.LoggerFromContext(ctx)
				log.Error("auth guard: session resolution failed", "error", err)
				if stateChangingMethods[r.Method] {
					writeError(w, r, http.StatusUnauthorized, authn.CodeUnauthenticated,
						"authentication required")
					return
				}
			}
		}
		r = r.WithContext(ctx)

		if stateChangingMethods[r.Method] {
			key := r.Method + " " + r.URL.Path
			if exemptWrites[key] {
				if !g.checkOrigin(w, r) || !g.checkJSONBody(w, r) {
					return
				}
			} else {
				p, ok := principalFrom(ctx)
				if !ok {
					// Structural 401 for unauthenticated writes — the
					// T0101 acceptance criterion lives here, before
					// routing: an unimplemented product endpoint answers
					// 401, not 404, when the caller is anonymous.
					logAuthFailure(r, authn.CodeUnauthenticated)
					writeError(w, r, http.StatusUnauthorized, authn.CodeUnauthenticated,
						"authentication required")
					return
				}
				if !g.checkCSRF(w, r, p.Session.CSRFToken) {
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// checkCSRF enforces the session-bound token on state changes. The
// comparison is constant-time; a missing header is the same 403 as a
// wrong one (no oracle).
func (g *guard) checkCSRF(w http.ResponseWriter, r *http.Request, want string) bool {
	got := r.Header.Get(headerCSRF)
	if got == "" || len(got) > 128 {
		logAuthFailure(r, authn.CodeCSRFFailed)
		writeError(w, r, http.StatusForbidden, authn.CodeCSRFFailed,
			"missing or invalid CSRF token")
		return false
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		logAuthFailure(r, authn.CodeCSRFFailed)
		writeError(w, r, http.StatusForbidden, authn.CodeCSRFFailed,
			"missing or invalid CSRF token")
		return false
	}
	return true
}

// checkOrigin blocks cross-site state changes on pre-session endpoints
// (login/signup): a browser always sends Origin on a cross-site POST, and
// SameSite=Lax already drops the cookie for subresource POSTs. Non-browser
// clients without an Origin header pass — they cannot be CSRF'd.
//
// The allowlist has exactly two entries: the request's own host, and the
// configured WebOrigin. The second exists because the default topology is
// cross-origin — the web app is served from one origin (127.0.0.1:3000)
// while the API listens on another port, so the same-host rule alone would
// refuse the login/signup of the web app this guard ships with. Only the
// configured origin is allowed: never an echo of the request Origin, never
// "*", and an unparseable origin never passes. This relaxation applies
// ONLY to the pre-session endpoints (checkOrigin is called exclusively
// from the exempt-writes branch); session-bearing writes stay protected
// by the session-bound CSRF token.
func (g *guard) checkOrigin(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get(headerOrigin)
	if origin == "" || origin == "null" {
		if origin == "null" {
			// Sandboxed/private-context browsers send the literal "null":
			// treat like an untrusted origin, not like a curl client.
			logAuthFailure(r, authn.CodeCSRFFailed)
			writeError(w, r, http.StatusForbidden, authn.CodeCSRFFailed,
				"cross-site request rejected")
			return false
		}
		return true
	}
	if webOriginMatches(g.cfg.WebOrigin, origin) {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host != r.Host {
		logAuthFailure(r, authn.CodeCSRFFailed)
		writeError(w, r, http.StatusForbidden, authn.CodeCSRFFailed,
			"cross-site request rejected")
		return false
	}
	return true
}

// webOriginMatches reports whether origin is exactly the configured web
// origin: same scheme and host (case-insensitive), no path. Empty config
// never matches — without a configured WebOrigin the check stays strict
// (same host only).
func webOriginMatches(configured, origin string) bool {
	if configured == "" {
		return false
	}
	wu, err := url.Parse(configured)
	if err != nil || wu.Host == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if u.Path != "" && u.Path != "/" {
		return false
	}
	return strings.EqualFold(u.Scheme, wu.Scheme) && strings.EqualFold(u.Host, wu.Host)
}

// checkJSONBody requires an application/json content type on state changes
// that carry a body — a cross-site HTML form cannot produce it (it also
// cannot set X-CSRF-Token). Bodyless requests (logout) skip the check.
func (g *guard) checkJSONBody(w http.ResponseWriter, r *http.Request) bool {
	if r.ContentLength == 0 && r.Header.Get("Transfer-Encoding") == "" {
		return true
	}
	ct := r.Header.Get(headerCT)
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil || mediaType != "application/json" {
		logAuthFailure(r, authn.CodeCSRFFailed)
		writeError(w, r, http.StatusUnsupportedMediaType, authn.CodeValidationFailed,
			"requests with a body must use Content-Type: application/json")
		return false
	}
	return true
}

// applyCORS answers preflights and stamps CORS headers for the configured
// web origin. It reports whether the request should continue (false =
// preflight answered, nothing else to do).
func (g *guard) applyCORS(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get(headerOrigin)
	if origin != "" {
		u, err := url.Parse(origin)
		if err == nil && (u.Host == g.webHost) && (u.Scheme == "http" || u.Scheme == "https") {
			w.Header().Set(headerACOrigin, origin)
			w.Header().Set(headerACCred, "true")
			w.Header().Add(headerVary, headerOrigin)
		}
	}
	if r.Method == http.MethodOptions {
		w.Header().Set(headerACMethod, "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set(headerACHeader, strings.Join([]string{headerCT, headerCSRF, observability.HeaderCorrelationID}, ", "))
		w.WriteHeader(http.StatusNoContent)
		return false
	}
	return true
}

// sessionCookie builds the session cookie. Secure is on in prod only: a
// prod cookie over plain http would never be set (safe failure — login
// breaks rather than leaking the token).
func sessionCookie(value string, ttlSeconds int, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     cookieSession,
		Value:    value,
		Path:     "/",
		MaxAge:   ttlSeconds,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
}

func clearSessionCookie(secure bool) *http.Cookie {
	c := sessionCookie("", -1, secure)
	return c
}

// oidcStateCookie binds the OIDC flow to the browser that started it. An
// empty state deletes the cookie (Max-Age<0): the flow is consumed or
// failed, so the binding must not linger.
func oidcStateCookie(state string, secure bool) *http.Cookie {
	maxAge := 600
	if state == "" {
		maxAge = -1
	}
	return &http.Cookie{
		Name:     cookieOIDC,
		Value:    state,
		Path:     "/api/v1/auth/oidc",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// clientIP extracts the client address for rate limiting (direct conn or
// the first proxy hop; V1 has no trusted-proxy config).
func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	return host
}
