// Package planner turns a natural-language research question into a
// structured search plan (T0903, docs/14 §2).
//
// The pipeline it implements is the spec's first step and nothing else:
//
//	question ──► Provider (a port; bytes in, bytes out)
//	              │
//	              ├─ JSON ─► plan schema ─► identifier guard ─► Document
//	              │                                            (StatusPlanned)
//	              └─ any failure ─────────────────────────────► fallback
//	                                                           (StatusFallback)
//
// The division of labour behind that shape is docs/20 §10: the model does
// query planning and answer interpretation, it is not a source of truth.
// Everything a plan is allowed to contain, this package decides; everything
// the plan means, the retrieval layer decides. A plan carries filters and
// preferences and no entities at all — see plan.go, and
// internal/search/scope.go for the authorization context that retrieval
// applies underneath it.
//
// What is deliberately NOT here:
//
//   - Retrieval, graph traversal and ranking (T0904/T0905). This package
//     stops at the validated document; it reads no rows and ranks nothing.
//   - Answer generation and the POST /search route (T0906). specs/api's
//     contract for the route already exists; no route is mounted for it here,
//     because the route's handler is the answer layer's and mounting half of
//     it now would collide with that task rather than prepare for it.
//   - A real provider. Docs name no vendor and no model for planning, and
//     whether a question may leave the platform for a third-party service is
//     a product/privacy decision that no document in this repository settles
//     (docs/54 #7 borders it; CLAUDE.md §5.1 makes a new external dependency
//     a stop condition). So this round ships the port and the deterministic
//     fake (plannertest) that proves the port is satisfiable and testable
//     without a network. Wiring a real adapter is a separate, decided change.
//   - Persistence. A plan is not stored here. docs/22 §8 asks the server to
//     save the query plan, and that belongs with the thing being saved — a
//     search, whose public entry point is T0906's — so this task adds no
//     migration and no table.
//
// The provider-port trio follows the repository's most complete example, the
// authn one: the interface (ports.go here, internal/application/authn/ports.go
// there), the real adapter (none here yet; authn's is oidc.go) and the
// in-process fake shipped as a normal package so unit tests and the contract
// fixtures share it (plannertest here, internal/application/authn/oidctest
// there).
package planner
