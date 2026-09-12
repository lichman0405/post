package authn

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/lichman0405/post/internal/domain"
)

// Ports: every external capability the service needs is an interface here
// (docs/52: application orchestrates against ports; adapters live in
// internal/persistence). Test doubles implement the same ports, so the
// unit and e2e suites run without PostgreSQL.

// ErrSessionNotFound is returned by SessionStore.Get for an unknown or
// expired token. Callers treat it as "no session", never as an error.
var ErrSessionNotFound = errors.New("authn: session not found")

// Session is a server-side login session. The token is the bearer secret
// (cookie value); nothing about the session is derivable from it — all
// state lives in the store, which is why logout is a real revocation.
type Session struct {
	Token     string    `json:"token"`
	UserID    string    `json:"user_id"`
	CSRFToken string    `json:"csrf_token"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// UserRecord is the persistence view of an account: the domain user plus
// the credential the store keeps (password hash for email+password
// accounts; empty for OIDC-only accounts).
type UserRecord struct {
	User         domain.User
	PasswordHash string
}

// UserStore is the account port. It is deliberately credential-aware (not
// a plain repository): the application must never see how many stores back
// a login — email+password and OIDC accounts are one identity space.
type UserStore interface {
	// FindByEmail returns the record for a normalized email, or
	// ErrUserNotFound.
	FindByEmail(ctx context.Context, email string) (UserRecord, error)
	// GetByID returns the record for an id, or ErrUserNotFound.
	GetByID(ctx context.Context, id string) (UserRecord, error)
	// CreateWithPassword creates an account with the encoded password
	// hash. It fails with ErrEmailTaken when the email already exists.
	CreateWithPassword(ctx context.Context, email, passwordHash string, handle, displayName string) (domain.User, error)
	// CreateOIDC creates an account from a verified external identity
	// (handle may be derived). It fails with ErrEmailTaken when the email
	// already exists.
	CreateOIDC(ctx context.Context, email, handle, displayName string) (domain.User, error)
}

// ErrUserNotFound is returned by UserStore lookups for unknown accounts.
var ErrUserNotFound = errors.New("authn: user not found")

// ErrEmailTaken is returned by UserStore.Create* when the email is
// already registered.
var ErrEmailTaken = errors.New("authn: email already registered")

// SessionStore persists sessions with TTL. Implementations must expire
// entries without external help (Redis EXPIRE, in-memory sweep) so a
// forgotten cleanup cannot mint immortal sessions.
type SessionStore interface {
	// Create stores the session; a later Create with the same token
	// replaces it (login after login is the same session token refresh —
	// idempotent by token).
	Create(ctx context.Context, s Session, ttl time.Duration) error
	// Get returns the stored session or ErrSessionNotFound.
	Get(ctx context.Context, token string) (Session, error)
	// Delete revokes the session (logout). Deleting an unknown token is
	// not an error — logout is idempotent.
	Delete(ctx context.Context, token string) error
}

// RateLimiter is the fixed-window limiter port. Check reports whether one
// more attempt is allowed in the current window for bucket, and how long
// the caller must wait otherwise. A limiter error means the policy state
// is unknowable; the service fails closed on it.
type RateLimiter interface {
	Check(ctx context.Context, bucket string, limit int, window time.Duration) (allowed bool, retryAfter time.Duration, err error)
}

// OIDCClaims is the verified identity extracted from a provider id_token:
// the claims POST accepts as identity facts. EmailVerified is asserted by
// the provider; POST requires it true before linking anything.
type OIDCClaims struct {
	Subject           string
	Email             string
	EmailVerified     bool
	PreferredUsername string
	Name              string
}

// OIDCProvider abstracts an OpenID Connect provider (authorization code
// flow). The real implementation performs discovery, code exchange and
// RS256 id_token verification; the fake provider in oidctest implements
// the same port.
type OIDCProvider interface {
	// AuthorizeURL builds the provider's authorization URL for a new flow
	// with the given state. callbackURL is the API's own callback endpoint
	// (request-derived: the API origin is unknowable at startup).
	AuthorizeURL(ctx context.Context, callbackURL, state string) (string, error)
	// ExchangeCode exchanges an authorization code for verified claims.
	ExchangeCode(ctx context.Context, code, redirectURI string) (OIDCClaims, error)
}

// NewToken returns a fresh opaque token: 32 random bytes base64url —
// 256 bits of entropy, unguessable and unencodable-invalid (URL- and
// cookie-safe alphabet).
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("authn: session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
