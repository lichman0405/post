package authhttp

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
)

// The auth HTTP surface. Every handler is a thin translation: parse the
// request, call the Service, render cookies/envelope (docs/52: transport
// never contains policy). All routes live under /api/v1/auth; the
// contract is OpenAPI-first and the canonical spec addition is handed to
// the Supervisor in the RESULT (specs/api/** is outside Worker scope).

// userPayload is the client-visible user shape (no password hash, no
// disabled flag — nothing that helps enumeration).
type userPayload struct {
	ID          string `json:"id"`
	Handle      string `json:"handle"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
}

func userPayloadFromDomain(u domain.User) userPayload {
	return userPayload{
		ID:          u.ID,
		Handle:      u.Handle,
		Email:       u.Email,
		DisplayName: u.DisplayName,
	}
}

// handlers owns the auth routes.
type handlers struct {
	svc    *authn.Service
	cfg    authn.Config
	secure bool
	web    string // web origin for OIDC completion redirects
}

// sessionPayload is the response of login/signup/session: the user plus
// the CSRF token the client must echo on state changes.
type sessionPayload struct {
	User      userPayload `json:"user"`
	CSRFToken string      `json:"csrf_token"`
}

func (h *handlers) sessionResponse(w http.ResponseWriter, result authn.SignupResult, status int) {
	http.SetCookie(w, sessionCookie(result.Session.Token,
		int(time.Until(result.Session.ExpiresAt).Seconds()), h.secure))
	writeJSON(w, status, sessionPayload{
		User:      userPayloadFromDomain(result.User),
		CSRFToken: result.Session.CSRFToken,
	})
}

type credentialsRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
}

func decodeCredentials(w http.ResponseWriter, r *http.Request) (credentialsRequest, bool) {
	var req credentialsRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if err := dec.Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, authn.CodeValidationFailed,
			"request body must be valid JSON")
		return credentialsRequest{}, false
	}
	return req, true
}

// handleSignup: POST /api/v1/auth/signup
func (h *handlers) handleSignup(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeCredentials(w, r)
	if !ok {
		return
	}
	result, err := h.svc.Signup(r.Context(), req.Email, req.Password, req.Handle, req.DisplayName, clientIP(r))
	if err != nil {
		h.authError(w, r, err)
		return
	}
	h.sessionResponse(w, result, http.StatusCreated)
}

// handleLogin: POST /api/v1/auth/login — enumeration-safe by Service
// design; rate-limited per email and per IP.
func (h *handlers) handleLogin(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeCredentials(w, r)
	if !ok {
		return
	}
	result, err := h.svc.Login(r.Context(), req.Email, req.Password, clientIP(r))
	if err != nil {
		h.authError(w, r, err)
		return
	}
	h.sessionResponse(w, result, http.StatusOK)
}

// handleLogout: POST /api/v1/auth/logout — the guard already required a
// valid session + CSRF token; revocation is idempotent.
func (h *handlers) handleLogout(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if ok {
		if err := h.svc.Logout(r.Context(), p.Session.Token); err != nil {
			writeError(w, r, http.StatusServiceUnavailable, authn.CodeServiceUnavailable,
				"could not revoke the session")
			return
		}
	}
	http.SetCookie(w, clearSessionCookie(h.secure))
	w.WriteHeader(http.StatusNoContent)
}

// handleSession: GET /api/v1/auth/session — whoami. 401 without a session
// (the web app renders login state from this).
func (h *handlers) handleSession(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, authn.CodeUnauthenticated,
			"no active session")
		return
	}
	writeJSON(w, http.StatusOK, sessionPayload{
		User:      userPayloadFromDomain(p.User),
		CSRFToken: p.Session.CSRFToken,
	})
}

// handleOIDCAuthorize: GET /api/v1/auth/oidc/authorize-url — starts a
// flow: fresh state bound to the browser via HttpOnly cookie, and the
// authorize URL carries this request's callback endpoint as redirect_uri.
func (h *handlers) handleOIDCAuthorize(w http.ResponseWriter, r *http.Request) {
	redirectURI := schemeHost(r) + h.cfg.OIDC.RedirectPath
	url, state, err := h.svc.OIDCAuthorizeURL(r.Context(), redirectURI)
	if errors.Is(err, authn.ErrOIDCNotConfigured) {
		writeError(w, r, http.StatusNotImplemented, authn.CodeOIDCNotConfigured,
			"OIDC login is not configured for this deployment")
		return
	}
	if err != nil {
		h.authError(w, r, err)
		return
	}
	http.SetCookie(w, oidcStateCookie(state, h.secure))
	writeJSON(w, http.StatusOK, map[string]string{"authorize_url": url})
}

// handleOIDCCallback: GET /api/v1/auth/oidc/callback?code&state — the
// provider's redirect target. The state must match the flow cookie
// (constant-time; login-CSRF defense) before the code is exchanged.
func (h *handlers) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	// fail aborts the flow: delete the (consumed) state cookie and report
	// the reason to the web login page. It must NOT clear the session
	// cookie — any site could otherwise log a signed-in user out by
	// pointing them at this GET (logout-CSRF); a failed login attempt is
	// not a logout.
	fail := func(code string) {
		http.SetCookie(w, oidcStateCookie("", h.secure))
		http.Redirect(w, r, h.web+"/login?error="+code, http.StatusFound)
	}

	stateCookie, err := r.Cookie(cookieOIDC)
	if err != nil || stateCookie.Value == "" {
		fail("state_mismatch")
		return
	}
	queryState := r.URL.Query().Get("state")
	if subtle.ConstantTimeCompare([]byte(queryState), []byte(stateCookie.Value)) != 1 {
		fail("state_mismatch")
		return
	}
	http.SetCookie(w, oidcStateCookie("", h.secure)) // one-time use

	code := r.URL.Query().Get("code")
	if code == "" {
		fail("no_code")
		return
	}
	redirectURI := schemeHost(r) + h.cfg.OIDC.RedirectPath
	result, err := h.svc.OIDCLogin(r.Context(), code, redirectURI)
	if err != nil {
		if errors.Is(err, authn.ErrOIDCEmailNotVerified) {
			fail("email_not_verified")
			return
		}
		logAuthFailure(r, authn.CodeOIDCProviderFailed)
		fail("provider_failed")
		return
	}
	http.SetCookie(w, sessionCookie(result.Session.Token,
		int(time.Until(result.Session.ExpiresAt).Seconds()), h.secure))
	http.Redirect(w, r, h.web+"/", http.StatusFound)
}

// authError maps Service errors to the wire (docs/45: stable codes, no
// dependency detail, no enumeration). Unknown errors are logged with
// detail and answered with a generic envelope.
func (h *handlers) authError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, authn.ErrInvalidCredentials):
		logAuthFailure(r, authn.CodeInvalidCredentials)
		writeError(w, r, http.StatusUnauthorized, authn.CodeInvalidCredentials,
			"invalid email or password")
	case errors.Is(err, authn.ErrEmailTaken):
		writeError(w, r, http.StatusConflict, authn.CodeEmailTaken,
			"an account with this email already exists")
	case errors.Is(err, authn.ErrValidation):
		writeError(w, r, http.StatusBadRequest, authn.CodeValidationFailed, err.Error())
	case errors.Is(err, authn.ErrUnavailable), errors.Is(err, authn.ErrStore):
		writeError(w, r, http.StatusServiceUnavailable, authn.CodeServiceUnavailable,
			"authentication is temporarily unavailable")
	case errors.Is(err, authn.ErrOIDCProviderFailed):
		logAuthFailure(r, authn.CodeOIDCProviderFailed)
		writeError(w, r, http.StatusBadGateway, authn.CodeOIDCProviderFailed,
			"the identity provider failed; try again")
	case errors.Is(err, authn.ErrOIDCEmailNotVerified):
		writeError(w, r, http.StatusForbidden, authn.CodeOIDCEmailNotVerified,
			"the provider did not verify your email address")
	default:
		var rateErr *authn.RateLimitError
		if errors.As(err, &rateErr) {
			retry := int(rateErr.RetryAfter.Seconds())
			if retry < 1 {
				retry = 1
			}
			w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
			logAuthFailure(r, authn.CodeRateLimited)
			writeError(w, r, http.StatusTooManyRequests, authn.CodeRateLimited,
				"too many attempts; try again later")
			return
		}
		observability.LoggerFromContext(r.Context()).Error("auth handler: unexpected error", "error", err)
		writeError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR",
			"an internal error occurred")
	}
}

// schemeHost reconstructs the request's origin (for the OIDC redirect_uri
// the provider must have been configured with).
func schemeHost(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
