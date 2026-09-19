package planner

import "context"

// The planner's port. docs/20 §10 fixes the boundary this file draws: "LLM 只
// 做 query planning/answer interpretation，不做 source of truth。
// Embedding/LLM provider 通过 port abstraction." — planning is the one thing
// a model is allowed to do here, and it happens behind an interface
// (docs/52 §External adapters lists LLM among the adapters that must go
// through a port, and forbids an external provider's id from becoming a
// domain identity).
//
// The interface is deliberately provider-AGNOSTIC in the literal sense: no
// document in this repository names a vendor or a model for planning
// (docs/14 §2, docs/20 §10 name none), so this package names none either —
// not in the interface, not in its types, not in its errors. What a
// conforming adapter does with the request is its own business; the contract
// it must keep is only this file plus the schema in PlanSchema.
//
// The shape mirrors the repository's most complete port trio (authn):
// the interface here, the deterministic fake as a normal package
// (plannertest, the way authn's fake provider ships as oidctest), and all
// tests driving the real consumer against the fake.

// Provider turns a natural-language question into a plan DOCUMENT.
//
// The return type is bytes, not a struct, and that is the load-bearing
// decision of this interface: a provider cannot hand back a value the
// planner has not validated, because the planner does not accept values from
// it. Everything a provider says is JSON that must survive the schema in
// plan_schema.go before any of it is looked at (docs/14 §2's planner is a
// parsing step, not a trusted source; docs/32's mitigation for "Search 产生
// 看似科学的 hallucination" is "planner structured; answer only cited entity
// ids; fallback structured results").
//
// Errors are the provider's business; the planner decides what they mean. A
// provider returning an error, a document that does not validate, or a
// document carrying an entity identifier all end the same way — the plan
// falls back to structured results (see Planner.Plan) — so an adapter does
// not need to know the fallback policy, only to answer truthfully.
type Provider interface {
	// PlanQuery returns the plan document for req: a JSON object conforming
	// to req.PlanSchema. It must return promptly when ctx is done.
	PlanQuery(ctx context.Context, req Request) ([]byte, error)
}

// Request is one planning request: the question, and the vocabulary to answer
// in.
//
// What is NOT in it is the point. There is no principal, no user id, no
// project id, no scope and no candidate entity of any kind: a planner that
// was told who is asking would be a planner that could be asked to plan
// around authorization, and an adapter that received the searcher's project
// ids would have received the shape of the platform's private structure for
// no reason (docs/54 #1 and #7; docs/21 §9 keeps authorization in the query
// layer). The scope stays with the caller and is applied to the RETRIEVAL,
// which is where a row can actually be filtered — never to the question.
//
// It follows that a future field added here must be a fact about the question
// itself (its locale, a narrowing hint the user expressed), never a fact
// about the user.
type Request struct {
	// Query is the user's question, verbatim.
	Query string
	// PlanSchema is the plan schema document the answer must conform to
	// (PlanSchema()). It travels with the request so the adapter can ask a
	// provider for schema-constrained output without importing this package:
	// the vocabulary is the contract, and the provider is told it rather
	// than left to guess.
	PlanSchema []byte
}
