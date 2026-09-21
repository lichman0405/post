// Package security holds the outbound-surface guards for the API process
// (T1106): the byte-exit registry, the runtime probes that judge the
// headers real handlers emit, and the front-end render guard.
//
// The package's job is to be able to say NO. Three properties, each with a
// failure mode that is silent without it:
//
//  1. Every place in cmd/api that can hand bytes to a client is registered
//     with the exit kind it is allowed to be. A new write site — a new
//     handler, a new download, a new rendered page — fails the guard until
//     someone classifies it. Without this, "we audited the byte exits" is a
//     statement about the day the audit happened.
//  2. Each registered exit is driven through its real handler and the
//     headers it actually sends are judged (internal/security.JudgeExit),
//     both on the handler's own output and through the edge middleware.
//     A classification that stopped matching the code is caught here, at
//     the handler, before the edge has a chance to paper over it.
//  3. The web app's one raw-HTML injection point is a literal, and stays
//     one. A non-literal dangerouslySetInnerHTML is a stored-XSS sink, and
//     nothing else in the build notices.
//
// The tests read source (go/parser, no dependencies) and run handlers
// (net/http/httptest). Nothing here needs a database, a Redis, or the
// network — the process-level assertions live in owasp-smoke.sh beside
// this package, which is also the command the G3 job runs.
package security
