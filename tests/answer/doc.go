// Package answer holds the golden-fixture contract test for the
// evidence-backed answer generator (T0906): the required test "answer
// grounding tests".
//
// docs/24 §6 names "Search query plan/answer citation" as fixture-bearing, and
// this is the citation half. The unit suite
// (internal/search/answer/*_test.go) proves the RULES over values it builds in
// Go; this file pins the CONTRACT — a fixture is a whole case (the ranking the
// answer is written from, the retrieval's signal report, the provider's reply,
// and the exact bytes that come out) so that a change to the citation
// vocabulary, to a derived sentence or to the canonical rendering shows up in
// a review as the change itself.
//
// Two things are pinned here that a unit test cannot pin, and they are the two
// acceptance criteria of the task:
//
//   - "模型不可引用不存在 id". Each fixture may declare `forbidden` tokens —
//     identities the provider wrote that retrieval never returned — and the
//     test asserts those bytes appear NOWHERE in the answer the platform
//     would publish. A refusal that leaked the invented id into a limitation
//     or a source would fail here.
//   - "source click 可定位". Every source's href is in the golden bytes, and
//     the set of fixtures covers an addressable source of each kind and the
//     two kinds the platform has no address for.
//
// Regenerate deliberately after a vocabulary change with
//
//	go test ./tests/answer -run TestGolden -update
//
// and review the diff: a regenerated fixture is a claim that the new output is
// the intended one, so the diff is the review.
package answer
