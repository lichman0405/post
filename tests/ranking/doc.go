// Package ranking holds the golden-fixture contract test for the scientific
// ranking (T0905).
//
// docs/24 §6 names the artifacts that must have golden fixtures, and the
// ranking is an interface between two halves of the search pipeline: T0904
// hands it candidates, T0906 cites what comes back, and the ORDER it returns
// is the thing a person reads. A change to the factor vocabulary, to a
// ladder's arrangement or to the canonical rendering has to be visible in a
// review as a fixture diff rather than as a subtly different result list at
// runtime.
//
// Each fixture under testdata/ is a whole case: the request, the candidates,
// the fact sheet the store would answer with (and which versions it would
// NOT — see `out_of_scope`), and the exact bytes the ranker produces. The
// test drives the real ranker over a deterministic fake store, so what is
// pinned includes the level each fact sheet maps to, the reasons, the labels,
// the order and the rendering — not just the struct.
//
// Regenerate deliberately after a vocabulary change with
//
//	go test ./tests/ranking -run TestGolden -update
//
// and review the diff. A regenerated fixture is a claim that the new output
// is the intended one, so the diff is the review.
package ranking
