package security

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lichman0405/post/internal/observability"
)

// Limiter is the fixed-window limiter port. It is deliberately the same
// shape as authn.RateLimiter — Go interfaces are structural, so the
// production adapter (persistence.RedisRateLimiter) satisfies both without
// an adapter, and the login service and this middleware count in the same
// Redis keyspace.
//
// Check reports whether one more request fits in the current window for
// bucket, and how long the caller must wait otherwise. An error means the
// policy state is unknowable; every caller in this package fails closed on
// it.
type Limiter interface {
	Check(ctx context.Context, bucket string, limit int, window time.Duration) (bool, time.Duration, error)
}

// Error codes on the wire. CodeRateLimited and CodeServiceUnavailable are
// the same strings authn uses (internal/application/authn/errors.go): one
// code means one thing across the API, and this middleware is a second
// producer of the first of them.
const (
	CodeRateLimited        = "RATE_LIMITED"
	CodeServiceUnavailable = "SERVICE_UNAVAILABLE"

	// HeaderRetryAfter is RFC 9110's delta-seconds field.
	HeaderRetryAfter = "Retry-After"
)

// Class is the traffic class a request falls into. Each class has its own
// budget: a request that forgets a password and a request that reads a
// public page are not the same risk, and one shared number would have to
// be either useless for the first or hostile to the second.
type Class string

const (
	// ClassCredential: the pre-session credential endpoints
	// (POST /api/v1/auth/login, POST /api/v1/auth/signup). Each call
	// burns argon2id work plus a store round-trip, so this is the
	// brute-force and CPU-DoS surface (docs/23 §7 "login brute force
	// protection"). Keyed per client IP: the attack is rotating emails
	// from one host, which a per-email budget cannot see.
	ClassCredential Class = "credential"
	// ClassOutbound: routes that hand control to an outbound HTTP client
	// (today the webhook delivery retry). One request can cost a remote
	// round-trip, so the budget is small.
	ClassOutbound Class = "outbound"
	// ClassAuthenticated: a request carrying the session cookie. Keyed by
	// the session token, not the IP: one browser behind a shared NAT must
	// not consume another user's budget.
	ClassAuthenticated Class = "authenticated"
	// ClassAnonymous: everything else — the public read surface and every
	// route reachable before authentication.
	ClassAnonymous Class = "anonymous"
)

// RateLimitConfig is the whole policy. The zero value is unusable on
// purpose: every budget must be stated.
type RateLimitConfig struct {
	// Window is the fixed window every counter is seeded with, and the
	// value reported in Retry-After when a bucket is full.
	Window time.Duration
	// AnonymousPerIP bounds one client IP's anonymous traffic.
	AnonymousPerIP int
	// AuthenticatedPerSession bounds one session token's traffic.
	AuthenticatedPerSession int
	// CredentialPerIP bounds login/signup attempts from one IP. It sits
	// ON TOP of authn's own per-email and per-IP budgets (which stay where
	// they are): the coarse edge budget is what protects the endpoint
	// before any of the login logic runs.
	CredentialPerIP int
	// OutboundPerKey bounds outbound-triggering requests.
	OutboundPerKey int
	// SessionCookie is the session cookie's name — the middleware reads it
	// to tell authenticated traffic from anonymous traffic. It is passed
	// in (cmd/api supplies authhttp's name) rather than guessed, so the
	// two can never disagree about which cookie is the session.
	SessionCookie string
	// ExemptPaths are exact request paths served without a limiter check.
	// Only the liveness/readiness probes are on it (see ExemptProbePaths).
	ExemptPaths []string
	// Clock is the limiter's notion of "now" only through the Limiter
	// port; nothing here reads the clock, which keeps the middleware
	// deterministic in tests.
}

// ExemptProbePaths are the paths the edge never rate limits.
//
// This is an availability decision, and it is the only exemption: a
// limiter whose counter lives in Redis must not be able to make an
// orchestrator believe every API replica is unhealthy. /healthz and
// /readyz are constant-cost handlers — no database, no upstream, no
// unbounded allocation — and what a flood of them costs (a TCP connection
// and a few microseconds) is not what a handler-chain limiter bounds
// anyway. Every product route, including the provider webhook receiver and
// the scaffold enqueue endpoint, is limited.
func ExemptProbePaths() []string {
	return []string{"/healthz", "/readyz"}
}

// RateLimit returns the middleware. Classify, count, and refuse: an
// over-budget request answers 429 with Retry-After and never reaches the
// handler.
//
// The middleware runs in front of the session guard on purpose — it must
// see traffic that has no session yet (that is where login brute force
// arrives), and it must not depend on the guard's store round-trip to
// decide a budget.
func RateLimit(l Limiter, cfg RateLimitConfig) func(http.Handler) http.Handler {
	exempt := make(map[string]bool, len(cfg.ExemptPaths))
	for _, p := range cfg.ExemptPaths {
		exempt[p] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if exempt[r.URL.Path] {
				next.ServeHTTP(w, r)
				return
			}
			session := sessionToken(r, cfg.SessionCookie)
			class := classify(r, session)

			bucket, limit := budget(class, r, session, cfg)
			allowed, retryAfter, err := l.Check(r.Context(), bucket, limit, cfg.Window)
			if err != nil {
				// Fail closed (docs/23 §3 "默认 deny"): an unreachable or
				// erroring limiter means the policy state is unknowable,
				// so the request is refused, never waved through. The
				// status is 503, the same outcome authn's service returns
				// when its limiter fails — the client learns "try later",
				// the log learns why.
				log := observability.LoggerFromContext(r.Context())
				log.Error("security: rate limiter failed; request refused",
					"error", err, "class", string(class), "method", r.Method, "path", r.URL.Path)
				writeEnvelope(w, r, http.StatusServiceUnavailable, CodeServiceUnavailable,
					"the service is temporarily unavailable; try again later")
				return
			}
			if !allowed {
				retry := int(retryAfter.Seconds())
				if retry < 1 {
					retry = 1
				}
				w.Header().Set(HeaderRetryAfter, strconv.Itoa(retry))
				log := observability.LoggerFromContext(r.Context())
				log.Warn("security: request rate limited",
					"class", string(class), "method", r.Method, "path", r.URL.Path,
					"retry_after_seconds", retry)
				writeEnvelope(w, r, http.StatusTooManyRequests, CodeRateLimited,
					"too many attempts; try again later")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// classify assigns the traffic class. Credential and outbound routes win
// over the session distinction: a logged-in user hitting the login
// endpoint still spends the credential budget.
func classify(r *http.Request, session string) Class {
	switch {
	case isCredentialRoute(r):
		return ClassCredential
	case isOutboundTriggerRoute(r):
		return ClassOutbound
	case session != "":
		return ClassAuthenticated
	default:
		return ClassAnonymous
	}
}

// isCredentialRoute matches the pre-session credential POSTs. Method
// matters: GET /api/v1/auth/session and the OIDC redirects are ordinary
// reads and belong in the anonymous budget.
func isCredentialRoute(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	switch r.URL.Path {
	case "/api/v1/auth/login", "/api/v1/auth/signup":
		return true
	default:
		return false
	}
}

// isOutboundTriggerRoute matches the routes that hand control to an
// outbound HTTP client: today exactly one is registered — the webhook
// delivery retry (POST /api/v1/webhooks/{id}/deliveries/{id}/retry), which
// makes the API perform a signed POST to a URL the caller chose.
//
// The external-reference refresh (internal/rsg/externalref) is the other
// outbound fetch surface in this tree, and it has NO HTTP route today
// (nothing under cmd/ imports the package; the only caller is the
// integration suite). When a route for it lands, it belongs here — and
// the guard test in tests/security fails until it is added.
func isOutboundTriggerRoute(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	p := r.URL.Path
	return strings.HasPrefix(p, "/api/v1/webhooks/") && strings.HasSuffix(p, "/retry")
}

// budget returns the counter key and its limit for one request.
//
// Keys are namespaced "edge:" so they cannot collide with the authn
// service's own buckets ("login:ip:...", "signup:ip:...") in the shared
// Redis keyspace — the two are complementary budgets, not one budget
// counted twice.
func budget(class Class, r *http.Request, session string, cfg RateLimitConfig) (string, int) {
	switch class {
	case ClassCredential:
		return "edge:cred:ip:" + ClientIP(r), cfg.CredentialPerIP
	case ClassOutbound:
		return "edge:out:" + keySuffix(r, session), cfg.OutboundPerKey
	case ClassAuthenticated:
		return "edge:auth:sess:" + tokenHash(session), cfg.AuthenticatedPerSession
	default:
		return "edge:anon:ip:" + ClientIP(r), cfg.AnonymousPerIP
	}
}

// keySuffix is the caller identity for classes that do not split on the
// session/anonymous distinction: the session token when there is one, the
// client IP otherwise.
func keySuffix(r *http.Request, session string) string {
	if session != "" {
		return "sess:" + tokenHash(session)
	}
	return "ip:" + ClientIP(r)
}

// sessionToken reads the session cookie's value, or "" when the request
// carries none. Nothing here validates the token: the guard does that a
// layer in, and a forged cookie simply buys the forger a bucket of its
// own instead of its IP's — which is the safe direction (it can never
// share a victim's budget, and the guard refuses it right afterwards).
func sessionToken(r *http.Request, cookieName string) string {
	if cookieName == "" {
		return ""
	}
	c, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// tokenHash derives the bucket suffix from a session token. The token is a
// bearer secret; hashing it keeps the secret out of Redis key names, out
// of any key listing, and out of a log line that ever prints a bucket. A
// truncated SHA-256 is plenty for a counter key: the value only has to be
// stable and collision-free, not preimage-resistant.
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:32]
}

// ClientIP is the client address the edge keys anonymous budgets on: the
// direct connection's host. V1 terminates on one server and has no
// trusted-proxy configuration, so a forwarded header is NEVER read here —
// honouring X-Forwarded-For without a trust list would let any client
// choose its own rate-limit bucket.
func ClientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	if host == "" {
		return "unknown"
	}
	return host
}

// errorEnvelope is the API's one error shape (docs/22 §5, docs/45):
// code, message, request id, retryable. It is byte-identical to
// cmd/api/authhttp's envelope — rendered here a second time because
// internal/** must not import cmd/**, and the two are pinned together by
// a test that compares this encoder's output with the documented shape.
type errorEnvelope struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Retryable bool   `json:"retryable"`
}

// writeEnvelope renders the standard error envelope. Messages are fixed
// strings by construction: a limiter's own error text (which can name a
// Redis address) never reaches a client.
func writeEnvelope(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	// The limiter's own refusals are byte exits too: a 429 is a JSON
	// document a browser renders, and it is produced here rather than by any
	// product handler, so it states its own nosniff.
	w.Header().Set(HeaderContentTypeOptions, ValueNosniff)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{
		Code:      code,
		Message:   message,
		RequestID: requestID(r),
		Retryable: status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests,
	})
}

func requestID(r *http.Request) string {
	id, ok := observability.FromContext(r.Context())
	if !ok {
		return ""
	}
	return id.String()
}

// String makes a class printable in a log line and in a test failure.
func (c Class) String() string { return string(c) }

// Describe renders the whole policy on one line. Startup logs use it so
// the running budgets are visible without reading the config loader.
func (cfg RateLimitConfig) Describe() string {
	return fmt.Sprintf("window=%s anonymous/ip=%d authenticated/session=%d credential/ip=%d outbound/key=%d exempt=%s",
		cfg.Window, cfg.AnonymousPerIP, cfg.AuthenticatedPerSession, cfg.CredentialPerIP,
		cfg.OutboundPerKey, strings.Join(cfg.ExemptPaths, ","))
}
