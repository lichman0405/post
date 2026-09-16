// Package explorehttp is the transport of the Explore surface (T0802):
//
//	GET /api/v1/explore
//	     The aggregated public index: the six dimensions of docs/05 §6
//
// # Anonymous by construction
//
// The route is a READ, and a read of the network's public entities: the
// shared /api/v1 guard resolves whatever session is present and lets an
// unauthenticated GET through (cmd/api/authhttp), and the handler asks for
// no principal at all. That is not an omission — the answer has no
// per-caller variant to compute, exactly like the asset hub's browse list
// (T0709): every section is built from a read whose own rule decides what
// is public, and the index renders what those reads resolved.
//
// docs/02 §3 makes anonymous public browsing a product promise
// ("Explore Public Projects/Assets/Knowledge/People/Organizations",
// "Public web access without login"), and docs/05 §1 repeats it for the
// surface list this route serves.
//
// # The path is not in the API contract
//
// specs/api/openapi.yaml is the Supervisor's document (it is not in this
// task's allowed scope) and it declares no aggregate read; the read routes
// this platform has added without a contract entry took the same route —
// docs/22's contract covers the surfaces that were specified with it, and
// T0505's provenance reads ("provenance" appears zero times in the
// contract) are the precedent this one follows. The route is mounted on the
// v1 mux directly, like every other product surface.
//
// # Where the pieces live
//
// The rules and the model are internal/application/explore (pure, and
// unit-tested there); the PostgreSQL adapter for the three sections that had
// no read before this task is store.go in this package (T0802's allowed
// scope excludes internal/persistence/**, so the adapter travels with the
// transport — the same arrangement T0505 recorded); the adapters that reuse
// the platform's existing public reads are adapters.go.
package explorehttp
