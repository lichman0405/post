// Package retrieval turns a plan and a scope into version-pinned candidates
// (T0904, docs/14 §2).
//
// The pipeline this package implements is the spec's middle step:
//
//	plan + scope ──► retrieval ──────────────► candidates (version-pinned)
//	                 │
//	                 ├─ full text      SearchDocumentsFullText
//	                 ├─ vector         SearchDocumentsByVector
//	                 ├─ facets         SearchDocumentsByFacets
//	                 └─ graph traversal ListScopeAdjacentRelationVersions
//	                                   ListScopeObjectVersions
//
// docs/14 §2 names the four recall signals ("structured filters + FTS +
// semantic candidate retrieval + graph traversal") and ADR-005 keeps all
// four in PostgreSQL for V1. One signal is deliberately absent:
// "scientific ranking" is the step AFTER this one (T0905), so nothing here
// decides what is scientifically better — the fusion below is a recall
// device, and it is documented as one.
//
// # What a candidate is
//
// A citation pointer, not an answer: the entity kind, its identity, and the
// VERSION the citation resolves to (ADR-010: the answer cites
// platform-determined versions; examples/search-answer.example.json renders
// them "CLM-DEMO-001@2"). A candidate carries no content, no payload and no
// score of scientific quality — docs/22 §8's rule that the Answer Generator
// "只可引用 planner/retrieval 返回的 entity ids/version" is a restriction
// this package has to make possible, and it can only do that by returning
// identities rather than prose.
//
// # Authorization
//
// Every candidate is authorized where it is READ, never filtered afterwards
// (docs/21 §9: authorization is not "hide it after the query"). There are
// exactly two ways a candidate can exist:
//
//   - a search_documents row the caller's scope admits — the predicate
//     `(visibility = 'public' OR project_id = ANY(scope))` that
//     SearchDocuments has carried since the security review, repeated
//     verbatim in each of the three document queries; or
//
//   - a scientific object version reached by a graph hop, whose project AND
//     whose relation's project are in the caller's scope, enforced in SQL by
//     the hop query itself.
//
// The scope is therefore not a filter this package applies to a result — it
// is an argument the reads cannot be issued without, and the only
// constructor of it is search.ResolveScope (internal/search/scope.go). A
// zero Scope is refused here rather than read as "no projects, so the
// public rows" — that reading is the fail-open shape docs/12 forbids.
//
// The graph half is strictly narrower than the RSG query surface's
// per-project read gate (internal/application/rsg/query.go), and on purpose:
// that surface is reached with a project in hand, a search is reached with
// none, and CLAUDE.md §9.6 ("Publish controls visibility") means an
// unpublished object version is not the network's to read. A non-member
// expands into nothing. The public half of the graph stays searchable one
// surface up, because the projection indexes exactly the published things.
//
// # Determinism
//
// The same plan, scope and corpus produce the same candidate list in the
// same order, every time: every query has a total ORDER BY (rank then
// entity_ref; distance then entity_ref; entity_ref; id), the traversal is a
// level-synchronous BFS over sorted ids, and fusion breaks ties by
// (signal count, ref). Nothing here depends on map iteration order or on the
// order a database happens to return equal rows in — the ranking fixture
// (T0905) and the answer's reproducibility both need that to hold.
package retrieval
