// Package searchplan holds the golden-fixture contract test for the search
// query plan (T0903).
//
// docs/24 §6 names the artifacts that must have golden fixtures, and "Search
// query plan/answer citation" is one of them: the plan a question produces is
// an interface between two halves of the search pipeline, so a change to its
// vocabulary, its values or its rendering must be visible in a review as a
// fixture diff rather than as a subtly different document at runtime.
//
// The fixtures live under testdata/ and each one is a whole case: the
// question, the document the provider answered with, and the exact bytes the
// planner produces. The test drives the real planner over the deterministic
// fake provider (internal/search/planner/plannertest), so what is pinned
// includes the validation, the identifier guard and the canonical rendering —
// not just the struct.
//
// Regenerate deliberately after a vocabulary change with
//
//	go test ./tests/searchplan -run TestGolden -update
//
// and review the diff.
package searchplan
