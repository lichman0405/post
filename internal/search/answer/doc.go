// Package answer is the evidence-backed answer layer (T0906): the last step
// of a search, where the retrieved candidates become something a person
// reads.
//
// ADR-010 is the charter. "关键词/向量列表是底层 retrieval，不是主体验。
// Answer 必须引用平台确定版本实体，显示冲突和限制" — a search's primary
// artifact is an answer that cites versions of real entities and shows the
// conflicts and the limits of what those entities say, and the keyword list
// underneath is retrieval, not the product. docs/14 §4 fixes what the answer
// carries (direct answer, conditions, conflicting/limited evidence, source
// objects, underlying results); docs/22 §8 fixes what it may cite
// ("Answer Generator 只可引用 planner/retrieval 返回的 entity ids/version");
// docs/32's risk register pairs the hallucination scenario with its
// mitigation ("planner structured; answer only cited entity ids; fallback
// structured results"), and docs/31 Gate E states it as an acceptance
// condition ("Answer 引用确定版本实体；不得 hallucinate 不存在 source").
//
// # The three rules this package enforces, and where each one lives
//
//  1. An answer cites only what retrieval returned. The citation vocabulary
//     is the ref set of the ranking's result — the ranking hands over the
//     retrieval.Candidate it was computed for (result.go's Ranked.Candidate)
//     precisely so this layer cites what it was given rather than
//     reconstructing it — and grounding.go refuses any document that names
//     anything else. The refusal is TOTAL (the whole document is dropped, not
//     the offending citation): a summary with its citation cut out is a
//     sentence whose evidence has been deleted from under it.
//
//  2. An answer always carries limitations, conflicts and sources.
//     Limitations and conflicts are derived from the platform's own facts
//     (the ranking's factor levels, labels and reasons, and the retrieval's
//     signal report) and are present in every answer, including one no model
//     touched. The model is not asked to author them, and could not be
//     trusted if it were: docs/14 §4's "Agent summary 明确标注为 View，不创造
//     新 Claim" means a model may phrase what the sources show, not assert a
//     relation the platform has not recorded — and "X contradicts Y" is
//     exactly such a relation. So the model's document is a summary and a
//     citation list, and the evidence-shaped sections are the platform's.
//
//  3. A model that cannot be trusted costs the user the ANSWER, not the
//     SEARCH (docs/27 §SLO: "完整答案目标 < 10s，超时提供 structured results
//     fallback"). Every provider-shaped failure — no provider configured, an
//     error, a timeout, a document that violates the schema, a document that
//     cites something retrieval did not return — produces a FALLBACK answer
//     that says why and carries the structured result. Only the two
//     conditions a caller can act on (no resolved scope, no question) are
//     errors.
//
// # What this package does not do
//
// It reads no database row of its own: the candidates, their versions, their
// factor facts and their authorization all came from retrieval (T0904) and
// ranking (T0905) under the caller's resolved scope (internal/search/scope.go),
// and this layer neither widens that scope nor re-derives it. It also does
// not persist: the search record (docs/22 §8's saved query plan, selected
// entity ids and answer citations) is written by the surface that owns the
// request, which is why the store in this package takes the answer it is
// handed rather than producing one.
//
// It ships no provider adapter. Docs name no vendor and no model for answer
// generation, and whether a question and its retrieved titles may leave the
// platform for a third-party service is a product/privacy decision no
// document here settles (internal/search/planner/doc.go records the same
// boundary for planning). A deployment that has not answered it runs with no
// provider at all, which is a supported state and not a degraded one: every
// answer is then the structured fallback, which is the honest answer to
// "what does the network hold about this" that the platform can give without
// a model.
package answer
