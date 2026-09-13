package authn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/lichman0405/post/internal/domain"
)

// Service orchestrates authentication use cases (docs/52: application is
// the only layer that may mutate canonical state). All policy lives here;
// the HTTP surface in cmd/api only translates requests into Service calls
// and results into cookies/envelopes.
type Service struct {
	users    UserStore
	sessions SessionStore
	limiter  RateLimiter
	oidc     OIDCProvider  // nil when OIDC is not configured
	audit    AuditRecorder // nil disables auth audit recording
	cfg      Config
}

// NewService wires the service. oidc may be nil (OIDC disabled); the OIDC
// endpoints then answer CodeOIDCNotConfigured. audit may be nil — auth
// audit recording then stays off (tests, minimal deployments).
func NewService(users UserStore, sessions SessionStore, limiter RateLimiter, oidc OIDCProvider, audit AuditRecorder, cfg Config) *Service {
	return &Service{users: users, sessions: sessions, limiter: limiter, oidc: oidc, audit: audit, cfg: cfg}
}

// SignupResult is the outcome of a successful signup or login: the user
// and the fresh session (the handler turns the token into a cookie).
type SignupResult struct {
	User    domain.User
	Session Session
}

// Signup creates an email+password account and logs it in. Validation
// failures are deterministic and never leak whether the email exists;
// an existing email answers ErrEmailTaken (see CodeEmailTaken docs for
// the disclosure tradeoff). Like Login, attempts are rate-limited (per
// IP — each call burns the same argon2id work plus an INSERT, so an
// unlimited signup endpoint would be a trivial CPU/DB DoS), and a
// limiter failure fails closed.
func (s *Service) Signup(ctx context.Context, email, password, handle, displayName, ip string) (SignupResult, error) {
	allowed, retryAfter, err := s.limiter.Check(ctx, "signup:ip:"+ip, s.cfg.SignupLimitPerIP, s.cfg.LoginWindow)
	if err != nil {
		return SignupResult{}, fmt.Errorf("%w: rate limiter failed", ErrUnavailable)
	}
	if !allowed {
		return SignupResult{}, &RateLimitError{RetryAfter: retryAfter}
	}
	if !domain.ValidEmail(email) {
		return SignupResult{}, fmt.Errorf("%w: invalid email", ErrValidation)
	}
	if !domain.ValidPassword(password) {
		return SignupResult{}, fmt.Errorf("%w: password must be 10-256 characters", ErrValidation)
	}
	email = domain.NormalizeEmail(email)
	if handle == "" {
		handle = deriveHandle(email)
	} else if !domain.ValidHandle(handle) {
		return SignupResult{}, fmt.Errorf("%w: handle may only contain letters, digits, - and _ (max 64 characters)", ErrValidation)
	} else {
		handle = domain.NormalizeHandle(handle)
	}
	if displayName == "" {
		displayName = handle
	} else if !domain.ValidDisplayName(displayName) {
		// T0102: signup and profile update share the one display-name rule
		// (non-empty visible text, max 200 bytes); the handle length is
		// already bounded by ValidHandle / deriveHandle above.
		return SignupResult{}, fmt.Errorf("%w: display name must be 1-200 characters and contain visible text", ErrValidation)
	}
	hash, err := HashPassword(password)
	if err != nil {
		return SignupResult{}, err
	}
	user, err := s.users.CreateWithPassword(ctx, email, hash, handle, displayName)
	if err != nil {
		return SignupResult{}, wrapStoreError(err)
	}
	result, err := s.startSession(ctx, user)
	if err == nil {
		s.recordAuth(ctx, user.ID, domain.ActionAuthSignup, domain.ViaPassword,
			"user:"+user.ID, nil, map[string]any{"handle": user.Handle}, nil)
	}
	return result, err
}

// Login authenticates an email+password pair. The response is identical
// for "unknown email" and "wrong password" (status, body, work performed)
// — enumeration protection. Attempts are rate-limited per email and per
// IP; a limiter failure fails closed.
func (s *Service) Login(ctx context.Context, email, password, ip string) (SignupResult, error) {
	if !domain.ValidEmail(email) || password == "" {
		return SignupResult{}, fmt.Errorf("%w: invalid credentials", ErrInvalidCredentials)
	}
	email = domain.NormalizeEmail(email)

	for _, bucket := range []struct {
		key   string
		limit int
	}{
		{"login:email:" + email, s.cfg.LoginLimitPerEmail},
		{"login:ip:" + ip, s.cfg.LoginLimitPerIP},
	} {
		allowed, retryAfter, err := s.limiter.Check(ctx, bucket.key, bucket.limit, s.cfg.LoginWindow)
		if err != nil {
			return SignupResult{}, fmt.Errorf("%w: rate limiter failed", ErrUnavailable)
		}
		if !allowed {
			return SignupResult{}, &RateLimitError{RetryAfter: retryAfter}
		}
	}

	record, err := s.users.FindByEmail(ctx, email)
	switch {
	case errors.Is(err, ErrUserNotFound):
		// Burn the same argon2id work a real verification would, so the
		// two outcomes are timing-indistinguishable.
		_, _ = VerifyPassword(dummyHash, password)
		// The account is unknown: the failure is recorded with no actor.
		// The audit log is internal state, so recording a known-account
		// failure (below) cannot leak through the enumeration-safe
		// response.
		s.recordAuth(ctx, "", domain.ActionAuthLoginFailed, domain.ViaPassword,
			"", nil, nil, map[string]any{"reason": "invalid_credentials"})
		return SignupResult{}, fmt.Errorf("%w: invalid email or password", ErrInvalidCredentials)
	case err != nil:
		return SignupResult{}, wrapStoreError(err)
	}
	if record.User.Disabled() {
		// Burn the same argon2id work a real verification would, so a
		// disabled account is timing-indistinguishable from any other
		// login failure (a fast return here would be an enumeration
		// oracle for "registered account").
		_, _ = VerifyPassword(dummyHash, password)
		// reason is auditReasonAccountDisabled — the same word the
		// disabled-account OIDC branch records (review M3): downstream
		// analysis of disabled-account attempts sees one vocabulary
		// across channels. The audit log is internal state, so naming
		// the reason cannot leak through the enumeration-safe response.
		s.recordAuth(ctx, record.User.ID, domain.ActionAuthLoginFailed, domain.ViaPassword,
			"user:"+record.User.ID, nil, nil, map[string]any{"reason": auditReasonAccountDisabled})
		return SignupResult{}, fmt.Errorf("%w: invalid email or password", ErrInvalidCredentials)
	}
	ok, err := VerifyPassword(record.PasswordHash, password)
	if err != nil || !ok {
		// An empty hash (OIDC-only account, NULL column) or a malformed
		// stored hash fails instantly; burn the same work a real
		// verification would so "registered OIDC account" is
		// timing-indistinguishable from "no account" too.
		if record.PasswordHash == "" || err != nil {
			_, _ = VerifyPassword(dummyHash, password)
		}
		s.recordAuth(ctx, record.User.ID, domain.ActionAuthLoginFailed, domain.ViaPassword,
			"user:"+record.User.ID, nil, nil, map[string]any{"reason": "invalid_credentials"})
		return SignupResult{}, fmt.Errorf("%w: invalid email or password", ErrInvalidCredentials)
	}
	result, err := s.startSession(ctx, record.User)
	if err == nil {
		s.recordAuth(ctx, record.User.ID, domain.ActionAuthLoginSuccess, domain.ViaPassword,
			"user:"+record.User.ID, nil, nil, nil)
	}
	return result, err
}

// Logout revokes the session. Revoking an unknown session is not an
// error (idempotent logout; the cookie is cleared either way). The audit
// record names the actor from the request context (the guard resolves the
// principal before the handler runs).
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	if err := s.sessions.Delete(ctx, token); err != nil {
		return err
	}
	actor := ""
	if info, ok := domain.RequestInfoFrom(ctx); ok {
		actor = info.ActorID
	}
	s.recordAuth(ctx, actor, domain.ActionAuthLogout, domain.ViaSession, "", nil, nil, nil)
	return nil
}

// Authenticate resolves a session token to the current user. It reloads
// the user on every request so a disabled account stops working
// immediately; missing/expired sessions answer ErrSessionNotFound.
func (s *Service) Authenticate(ctx context.Context, token string) (domain.User, Session, error) {
	if token == "" {
		return domain.User{}, Session{}, ErrSessionNotFound
	}
	sess, err := s.sessions.Get(ctx, token)
	if err != nil {
		return domain.User{}, Session{}, err
	}
	record, err := s.users.GetByID(ctx, sess.UserID)
	if errors.Is(err, ErrUserNotFound) {
		// Orphaned session (account data gone): treat as no session.
		return domain.User{}, Session{}, ErrSessionNotFound
	}
	if err != nil {
		return domain.User{}, Session{}, wrapStoreError(err)
	}
	if record.User.Disabled() {
		return domain.User{}, Session{}, ErrSessionNotFound
	}
	return record.User, sess, nil
}

// OIDCConfig exposes the deployment's OIDC wiring state to the handler
// (authorize-url answers 501 when disabled).
func (s *Service) OIDCConfig() OIDCConfig { return s.cfg.OIDC }

// OIDCAuthorizeURL starts an OIDC flow: a fresh random state and the
// provider's authorization URL carrying the request-derived callback URL.
func (s *Service) OIDCAuthorizeURL(ctx context.Context, callbackURL string) (url, state string, err error) {
	if s.oidc == nil || !s.cfg.OIDC.Enabled {
		return "", "", ErrOIDCNotConfigured
	}
	state, err = NewToken()
	if err != nil {
		return "", "", err
	}
	url, err = s.oidc.AuthorizeURL(ctx, callbackURL, state)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrOIDCProviderFailed, err)
	}
	return url, state, nil
}

// OIDCLogin completes an OIDC flow: code exchange + id_token verification
// happened inside the provider port; the returned claims are identity
// facts. POST only ever links on a verified email (no account takeover by
// unverified claims) and creates the account on first login.
func (s *Service) OIDCLogin(ctx context.Context, code, redirectURI string) (SignupResult, error) {
	if s.oidc == nil || !s.cfg.OIDC.Enabled {
		return SignupResult{}, ErrOIDCNotConfigured
	}
	claims, err := s.oidc.ExchangeCode(ctx, code, redirectURI)
	if err != nil {
		return SignupResult{}, fmt.Errorf("%w: %v", ErrOIDCProviderFailed, err)
	}
	if !domain.ValidEmail(claims.Email) {
		return SignupResult{}, fmt.Errorf("%w: provider returned no usable email", ErrOIDCProviderFailed)
	}
	if !claims.EmailVerified {
		return SignupResult{}, fmt.Errorf("%w: provider did not verify the email", ErrOIDCEmailNotVerified)
	}
	email := domain.NormalizeEmail(claims.Email)

	record, err := s.users.FindByEmail(ctx, email)
	switch {
	case errors.Is(err, ErrUserNotFound):
		user, err := s.users.CreateOIDC(ctx, email, oidcHandle(claims, email), oidcDisplayName(claims, email))
		if err != nil {
			return SignupResult{}, wrapStoreError(err)
		}
		result, err := s.startSession(ctx, user)
		if err == nil {
			s.recordAuth(ctx, user.ID, domain.ActionAuthSignup, domain.ViaOIDC,
				"user:"+user.ID, nil, map[string]any{"handle": user.Handle}, nil)
			s.recordAuth(ctx, user.ID, domain.ActionAuthLoginSuccess, domain.ViaOIDC,
				"user:"+user.ID, nil, nil, nil)
		}
		return result, err
	case err != nil:
		return SignupResult{}, wrapStoreError(err)
	}
	if record.User.Disabled() {
		s.recordAuth(ctx, record.User.ID, domain.ActionAuthLoginFailed, domain.ViaOIDC,
			"user:"+record.User.ID, nil, nil, map[string]any{"reason": auditReasonAccountDisabled})
		return SignupResult{}, fmt.Errorf("%w: account disabled", ErrInvalidCredentials)
	}
	result, err := s.startSession(ctx, record.User)
	if err == nil {
		s.recordAuth(ctx, record.User.ID, domain.ActionAuthLoginSuccess, domain.ViaOIDC,
			"user:"+record.User.ID, nil, nil, nil)
	}
	return result, err
}

// startSession mints a fresh session for a user: new token, fresh CSRF
// token, stored with the configured TTL.
func (s *Service) startSession(ctx context.Context, user domain.User) (SignupResult, error) {
	token, err := NewToken()
	if err != nil {
		return SignupResult{}, err
	}
	csrf, err := NewToken()
	if err != nil {
		return SignupResult{}, err
	}
	now := time.Now()
	sess := Session{
		Token:     token,
		UserID:    user.ID,
		CSRFToken: csrf,
		CreatedAt: now,
		ExpiresAt: now.Add(s.cfg.SessionTTL),
	}
	if err := s.sessions.Create(ctx, sess, s.cfg.SessionTTL); err != nil {
		return SignupResult{}, err
	}
	return SignupResult{User: user, Session: sess}, nil
}

// recordAuth appends one auth audit entry (T0110). Recording is
// best-effort — an auth outcome must never fail because the audit store
// failed — but a failure is logged, never swallowed silently. The
// correlation id comes from the request context (the auth guard attaches
// it); the via and actor are explicit because this service knows which
// authentication method ran and which account it touched.
func (s *Service) recordAuth(ctx context.Context, actorID, action, via, targetRef string, before, after, metadata map[string]any) {
	if s.audit == nil {
		return
	}
	corr := ""
	if info, ok := domain.RequestInfoFrom(ctx); ok {
		corr = info.CorrelationID
	}
	err := s.audit.Record(ctx, domain.AuditEntry{
		ActorID:       actorID,
		Via:           via,
		Action:        action,
		TargetRef:     targetRef,
		CorrelationID: corr,
		BeforeSummary: before,
		AfterSummary:  after,
		Metadata:      metadata,
	})
	if err != nil {
		slog.Warn("authn: audit record failed", "action", action, "error", err)
	}
}

// auditReasonAccountDisabled is the one metadata reason for auth.login.failed
// rows caused by a disabled account, on every channel (password and OIDC):
// downstream analysis of disabled-account attempts sees a single vocabulary.
const auditReasonAccountDisabled = "account_disabled"

// Errors the handler maps to wire codes (each wraps a stable sentinel so
// callers use errors.Is).
var (
	ErrInvalidCredentials   = errors.New("authn: invalid credentials")
	ErrValidation           = errors.New("authn: validation failed")
	ErrUnavailable          = errors.New("authn: dependency unavailable")
	ErrStore                = errors.New("authn: store failure")
	ErrOIDCNotConfigured    = errors.New("authn: oidc not configured")
	ErrOIDCProviderFailed   = errors.New("authn: oidc provider failed")
	ErrOIDCEmailNotVerified = errors.New("authn: oidc email not verified")
)

// wrapStoreError normalizes store failures at the service boundary: the
// two expected domain outcomes (taken email, unknown user) pass through,
// everything else — including a persistence adapter that cannot run (e.g.
// the auth migration not yet applied) — becomes ErrStore for the handler,
// with the cause kept for the log.
func wrapStoreError(err error) error {
	if err == nil || errors.Is(err, ErrEmailTaken) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}

// RateLimitError carries how long the client must wait.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("authn: rate limited (retry after %s)", e.RetryAfter)
}

// deriveHandle builds a handle from an email local part (L1: signup may
// omit a handle; the identity table needs a unique one).
func deriveHandle(email string) string {
	local, _, _ := strings.Cut(email, "@")
	local = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '-'
	}, local)
	local = strings.Trim(local, "-")
	if local == "" {
		local = "user"
	}
	if len(local) > 40 {
		local = local[:40]
	}
	return local
}

func oidcHandle(claims OIDCClaims, email string) string {
	if claims.PreferredUsername != "" && len(claims.PreferredUsername) <= 40 {
		h := strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
				return r
			}
			return '-'
		}, claims.PreferredUsername)
		if strings.Trim(h, "-") != "" {
			return strings.Trim(h, "-")[:min(len(strings.Trim(h, "-")), 40)]
		}
	}
	return deriveHandle(email)
}

func oidcDisplayName(claims OIDCClaims, email string) string {
	if claims.Name != "" && len(claims.Name) <= 200 {
		return claims.Name
	}
	return oidcHandle(claims, email)
}
