package answer

import "context"

// The answer provider's port. docs/20 §10 fixes the boundary this file
// draws: "LLM 只做 query planning/answer interpretation，不做 source of
// truth。Embedding/LLM provider 通过 port abstraction." — interpreting
// retrieved evidence into a sentence is the second of the two things a model
// is allowed to do here, and it happens behind an interface.
//
// The interface is deliberately provider-AGNOSTIC in the literal sense: no
// document in this repository names a vendor or a model for answer writing,
// so this package names none either — not in the interface, not in its
// types, not in its errors. What a conforming adapter does with the request
// is its own business; the contract it must keep is this file plus the schema
// in schema.go.
//
// The shape mirrors the planner's port (internal/search/planner/ports.go),
// which mirrors the repository's most complete port trio, authn's: the
// interface here, a deterministic fake in the tests, and every test driving
// the real consumer.

// Provider turns a question and the entities retrieval returned into an
// answer DOCUMENT.
//
// The return type is bytes, not a struct, and that is the load-bearing
// decision of this interface: a provider cannot hand back a value this
// package has not validated, because this package does not accept values
// from it. Everything a provider says is JSON that must survive the schema
// in schema.go AND the grounding guard in grounding.go before any of it is
// looked at.
//
// Errors are the provider's business; the generator decides what they mean.
// A provider returning an error, a document that does not validate, or a
// document that cites an entity retrieval did not return all end the same
// way — the answer falls back to the structured result — so an adapter does
// not need to know the fallback policy, only to answer truthfully.
type Provider interface {
	// AnswerQuestion returns the answer document for req: a JSON object
	// conforming to req.AnswerSchema. It must return promptly when ctx is
	// done.
	AnswerQuestion(ctx context.Context, req Request) ([]byte, error)
}

// Request is one answer request: the question, and the only entities the
// answer may cite.
//
// What is NOT in it is the point, and the list is longer than for the
// planner's request because this is the step a model could leak through.
// There is no principal, no user id, no scope, no project MEMBERSHIP, and no
// entity of any kind that retrieval did not return: an adapter that received
// the searcher's project ids would have received the shape of the platform's
// private structure for no reason (docs/54 #1 and #7), and an adapter handed
// an entity outside Citable would be able to cite it. The scope stays with
// the caller and was applied to the RETRIEVAL, which is where a row can
// actually be filtered — never to the question.
//
// It follows that a future field added here must be a fact about the
// question or about a citable entity, never a fact about the user.
type Request struct {
	// Query is the user's question, verbatim.
	Query string
	// Citable is the citation vocabulary: one entry per candidate the
	// search returned, in rank order. An answer may cite these refs and no
	// others, and the guard enforces exactly that.
	Citable []CitableEntity
	// AnswerSchema is the answer schema document the answer must conform to
	// (AnswerSchema()). It travels with the request so the adapter can ask a
	// provider for schema-constrained output without importing this package:
	// the vocabulary is the contract, and the provider is told it rather
	// than left to guess.
	AnswerSchema []byte
}

// CitableEntity is one entity the provider MAY cite, with the facts the
// platform holds about it.
//
// The reasons are the ranking's own Assessment reasons, verbatim and in the
// ranking's priority order (query/scope, evidence, review, reproduction,
// conflict, version). Passing the ranking's sentences rather than a fresh
// summary of them is deliberate: an adapter that received a second,
// differently-worded account of a source's evidence could paraphrase a fact
// the ranking never stated, and there would then be two accounts of the same
// row in circulation.
//
// What is NOT here is the entity's content. A candidate carries a ref, a
// title and the facts the platform read about it; it does not carry the
// document text, and this layer does not read the row again to get it (see
// doc.go: this package reads no database row of its own). An answer written
// from titles and evidence facts is a view of what the network holds, which
// is what docs/14 §4 asks for; a summary that needs the full text of a
// figure or a method is a summary this pipeline cannot ground, and the
// sources are listed beside it for the reader to open.
type CitableEntity struct {
	// Ref is the citation identity. It is matched EXACTLY by the guard.
	Ref string `json:"ref"`
	// Kind is retrieval.KindDocument or retrieval.KindObjectVersion.
	Kind string `json:"kind"`
	// EntityType is the projection's entity type for a document.
	EntityType string `json:"entity_type,omitempty"`
	// ObjectType is scientific_objects.object_type when it is known.
	ObjectType string `json:"object_type,omitempty"`
	// Title is the entity's title.
	Title string `json:"title,omitempty"`
	// Version is the pinned version label.
	Version string `json:"version,omitempty"`
	// Labels are docs/10 §8's descriptive labels.
	Labels []string `json:"labels"`
	// Reasons are the ranking's factor reasons, in priority order.
	Reasons []string `json:"reasons"`
}
