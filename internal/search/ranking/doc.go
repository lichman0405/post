// Package ranking is T0905: the scientific ranking step of the search
// pipeline (docs/14 §3, ADR-005).
//
// docs/14 §2 fixes the pipeline as "structured filters + FTS + semantic
// candidate retrieval + graph traversal + scientific ranking". T0901 filled
// the projection, T0902 the vector, T0903 the plan, T0904 the recall; this
// package is the last step. It takes a retrieval.Result — the candidate set
// with its signal reports — and returns the SAME candidates in the order
// docs/14 §3 asks for, each one carrying the reasons for its position.
//
// # The six factors, and what is deliberately not among them
//
// docs/14 §3: "主要考虑 query/scope match、evidence profile、review state、
// independent reproduction、contradictory evidence、version/freshness。不得
// 主要按 popularity/star/organization prestige。"
//
// So the factor vocabulary is CLOSED and is exactly those six, in that
// priority order (Factors). There is no popularity factor, no star count, no
// follower count, no organization size, no author h-index, no "number of
// contributors" — not as a low-weighted term, and not as a tie-break. The
// absence is not an omission to be filled in later: it is the documented
// rule, and TestNoPrestigeFactor in this package's unit suite asserts that
// the vocabulary is closed at six, so a seventh factor cannot arrive
// unnoticed. tests/integration/ranking_test.go is the behavioural half: it
// seeds two versions with identical fact sheets in projects that differ only
// in the counts a popularity ranking would read — members, an organization,
// subscribers to the project, subscribers to the author, and how much the
// project has produced — and pins that the ranking does not separate them,
// whichever order the retrieval hands them over in.
//
// # Why levels, and not a weighted score
//
// Each factor returns a LEVEL — a named, ordered category like
// "independent" or "reviewed" — and never a number to be summed. Two
// reasons, and both are constraints rather than preferences:
//
//   - docs/10 §4: "V1 不自动赋数值权重". A weighted sum of evidence types is
//     a numerical weight the platform assigned to "reproduces" against
//     "supports" without a human deciding it. Levels need no such constant:
//     "an independent reproduction exists" is a fact, and that it beats "only
//     the origin project reproduced it" is a rule a reader can argue with.
//
//   - CLAUDE.md §9.13 and docs/10 §8 forbid a Truth Score. Any number that
//     orders candidates is read as an assessment of the science the moment a
//     caller sees it, and a caller will see it. RankedCandidate therefore
//     carries a POSITION (its index in the list) and its factor levels, and
//     deliberately no score: what is not representable cannot be rendered.
//
// The ordering is lexicographic over the six factors in priority order. It is
// total over the candidates the factors separate, and where they do not — two
// candidates identical on all six — the ranking KEEPS the order the retrieval
// returned, which is the fusion's own relevance order. It adds no tie-break of
// its own: inventing one (a ref, a title, an id) would be ordering two equally
// supported candidates by something the platform never claimed was a reason.
// The result is deterministic either way: the same (request, candidates,
// facts) always produces the same order, which is what the "ranking
// deterministic fixture" acceptance and tests/ranking's golden pin.
//
// # What it does not do
//
// It does not recall, does not authorize, and does not widen. Every candidate
// it returns came out of the retrieval, so every access rule T0904 enforced
// is upstream of here and unchanged; the ranking can only reorder what the
// scope already admitted. It also does not read the evidence graph of a
// project the caller's scope does not cover — see internal/search/ranking/
// store.go for why that is a refusal and not a gap.
package ranking
