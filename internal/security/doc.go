// Package security owns the API's edge hardening (docs/23 §7 "Web": CSP,
// secure headers, SameSite, rate limiting, login brute force protection).
//
// It is a transport-layer package: it knows nothing about the domain, it
// takes an http.Handler and returns one. Three pieces live here.
//
//   - Headers: the secure response-header set stamped on EVERY response
//     (CSP, nosniff, X-Frame-Options, Referrer-Policy, Permissions-Policy,
//     HSTS, a version-free Server token). The CSP is chosen from the
//     response's own Content-Type at WriteHeader time, because this API is
//     not a JSON-only API: cmd/api/rsghttp serves server-rendered HTML
//     pages on the same routes when a browser navigates (T0210/T0211), and
//     a single lock-everything CSP would leave those pages unstyled.
//
//   - Ratelimit: a fixed-window limiter in front of the whole tree,
//     keyed per client IP for anonymous traffic and per session token for
//     authenticated traffic. The limiter port is structurally identical to
//     authn.RateLimiter, so persistence.RedisRateLimiter serves both — the
//     point of this package is that the policy is reusable middleware
//     instead of being welded into the login service.
//
//   - Outbound: the contract for responses that hand payload bytes to a
//     browser (hardenedExit), enforced by the tests in tests/security.
//     docs/23 §6: "Content-Disposition 安全、HTML/SVG active content
//     sandbox、preview sanitization".
//
// Deliberately NOT here: CORS/CSRF/session policy (cmd/api/authhttp owns
// the /api/v1 guard) and the outbound-fetch SSRF guard
// (internal/rsg/externalref and internal/events own theirs, T0508). Both
// already exist; this package does not restate them.
package security
