// Package freezehttp is the freeze-governance HTTP surface (T0601): the
// one endpoint that freezes a project's main,
//
//	POST /api/v1/projects/{projectId}/main:freeze
//
// (specs/api/openapi.yaml:43-50, summary "Freeze main; governance action").
// The route carries no freeze logic: it resolves the actor and the
// Idempotency-Key, calls the mainfreeze command, and maps the command's own
// errors onto the wire envelope. Every rule about who may freeze, under
// which policy, and what a freeze must record lives behind that call.
//
// Two transport decisions are the contract's:
//
//   - Idempotency-Key is REQUIRED here (components.parameters.IdempotencyKey:
//     required, minLength 8). The freeze is a governance command docs/22 §3
//     lists among the ones a retry must be able to repeat safely; a client
//     that cannot name its request cannot have its retry answered by the
//     state it already produced, so the header is refused rather than
//     defaulted.
//   - the actor is the authenticated session, and the command's own
//     authorization (ActionFreezeMain) is what decides whether that actor
//     may freeze; the route never pre-judges it.
//
// There is no read gate in front of the command, and that is a decision
// rather than an omission. Every other project route runs the project read
// gate first (mergehttp does: a merge is exactly as reachable as its
// project). Here the acceptance criterion is the opposite one: an unknown
// project id must answer a PERMISSION-class refusal, not "project not
// found", because the authorization refusal is what must not disclose
// whether the project exists. The gate would answer 404 and disclose
// exactly that. The command's own authorization resolves the caller's
// membership — which is "none" for a project that does not exist — and
// refuses with AUTH_FORBIDDEN for both, so member and stranger, existing
// project and unknown id, are indistinguishable from the outside.
//
// The route is registered on the v1 mux like every other product route.
// The path needs no wildcard trick: only the {projectId} segment is a
// wildcard, and Go's ServeMux is happy with a literal suffix on the last
// segment here (unlike mergehttp's "{number}:merge", where the wildcard and
// the suffix share a segment).
package freezehttp
