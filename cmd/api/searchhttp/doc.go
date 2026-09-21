// Package searchhttp mounts POST /api/v1/search: the one route that runs the
// whole search pipeline and answers with an evidence-backed answer (T0906).
//
// # The pipeline, and why it lives here
//
// One request runs five steps in order, and each one's output is the next
// one's input:
//
//	search.ResolveScope  actor        -> project scope
//	planner.Planner      scope, query -> plan       (optional)
//	retrieval.Retriever  scope, plan  -> candidates + signal report
//	ranking.Ranker       scope, cands -> ranked result
//	answer.Generator     ranked, sigs -> answer
//
// The composition lives in this transport rather than in an application
// service because internal/application is not this task's to write and,
// more to the point, because there is nothing here to decide: the pipeline
// has no branch a product rule could hang on. What this package adds is the
// FOUR decisions that belong to a transport — what the request must contain,
// what a partial failure means, what gets recorded, and what the wire looks
// like — and each of them is documented where it is taken.
//
// # The two things this surface promises
//
//  1. Nothing reaches a caller that the platform cannot stand behind. An
//     answer cites entity versions the retrieval returned, or it is the
//     structured result; the coverage limitations and the conflicts are
//     derived from the ranking's own facts, never authored (docs/14 §4,
//     docs/32, docs/31 Gate E). A provider failure costs the written
//     sentence, never the results.
//
//  2. The search survives the response. docs/22 §8 requires the server to
//     save the query plan, the selected entity ids and the answer citations,
//     and the contract addresses a search by id afterwards
//     (POST /search/{searchId}:start-project). A search whose record could
//     not be written is therefore NOT answered: see service.go for why that
//     is a 503 rather than an answer with a dangling id.
//
// # What is not here
//
// No reader of a stored search (the start-project flow is T0908's), and no
// provider adapter: this package is handed a planner and an answer generator
// already wired, or handed nil, and nil is a supported state for both — a
// deployment that has not answered whether content may leave the platform
// still searches, because both steps lose only the sentence they would have
// written, never the structured result underneath it.
package searchhttp
