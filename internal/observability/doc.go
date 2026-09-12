// Package observability implements the P0 logging/correlation baseline
// (docs/26 §2): every request/command/job carries a correlation id that is
// created at the HTTP edge and survives across API -> queue -> worker and
// across the web app / scientific-adapter boundaries.
//
// Components:
//
//   - correlation ids: generation, format validation and context plumbing
//     (correlation.go). Incoming ids are validated before use — a header
//     value must match a strict format or the edge creates a fresh id.
//   - HTTP middleware: assigns the id, echoes it back to the caller, and
//     emits one structured request-completion line per request
//     (middleware.go). The request-scoped logger is attached to the
//     context, so handler code logs the id by calling LoggerFromContext.
//   - go-redis bridge: routes go-redis's internal logging (retry/connection
//     chatter, otherwise unformatted stdlib output) through slog, with
//     every message redacted via internal/config RedactForOutput
//     (redislog.go).
//
// Redaction: there is deliberately no second redactor in this package —
// everything emitted here goes through internal/config.RedactForOutput /
// RedactURL, the same redaction the config layer applies (T0004). Request
// logging never includes the query string (a common credential carrier).
//
// This is logging, not the domain audit log: the audit surface arrives with
// the event/audit tasks (docs/26 §1, §5).
package observability
