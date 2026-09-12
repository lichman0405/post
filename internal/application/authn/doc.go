// Package authn implements POST authentication and sessions (T0101):
// email+password signup/login with argon2id hashing, OIDC authorization
// code login with real id_token verification, server-side revocable
// sessions, CSRF-bound session tokens and login rate limiting.
//
// Layering (docs/52): this package is application-layer — it orchestrates
// use cases against ports (UserStore, SessionStore, RateLimiter,
// OIDCProvider) and never imports concrete infrastructure (no Redis, no
// pgx, no HTTP). Infrastructure adapters live in internal/persistence; the
// HTTP surface lives in cmd/api.
//
// Security posture (docs/23, docs/54):
//   - login responses are identical for "unknown account" and "wrong
//     password", and both paths run the same argon2id work (a fixed dummy
//     hash for unknown emails) — no account enumeration via login;
//   - sessions are opaque 256-bit random tokens stored server-side with a
//     TTL; logout revokes them; cookies are HttpOnly + SameSite=Lax and
//     Secure in prod;
//   - every session carries a CSRF token that state-changing requests must
//     echo in X-CSRF-Token;
//   - login is rate-limited per email and per IP (fail-closed when the
//     limiter errors).
package authn
