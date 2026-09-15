// Package mergehttp is the merge-governance HTTP surface (T0409): the one
// endpoint that advances a project's frozen main,
//
//	POST /api/v1/projects/{projectId}/pull-requests/{number}:merge
//
// (specs/api/openapi.yaml). The route carries no merge logic: it resolves the
// actor and the Idempotency-Key, calls the T0406 semantic merge command, and
// maps the command's own errors onto the wire envelope. Every rule about what
// may merge — the PR's state, the plan, the governance policy, the integrity
// review, the ref update — lives behind that call.
//
// Two transport decisions are the contract's:
//
//   - Idempotency-Key is REQUIRED here (minLength 8, the components.parameters
//     entry the route references). The merge moves frozen main, and docs/22 §3
//     lists it among the commands a retry must be able to repeat safely; a
//     client that cannot name its request cannot be replayed onto the merge it
//     already produced, so the header is refused rather than defaulted.
//   - the actor is the authenticated session, and the command's own
//     authorization (ActionMergeMain) is what decides whether that actor may
//     merge; the route never pre-judges it.
//
// Path parsing: Go's ServeMux rejects a partial-segment wildcard
// ("{number}:merge" panics with "bad wildcard segment"), so the segment is
// registered as a remainder wildcard and the ":merge" suffix is split off in
// the handler — the same technique the branch :validate route uses. A path
// without that suffix was routed here by prefix only and is a 404.
package mergehttp
