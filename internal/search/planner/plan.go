package planner

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// This file is the plan DOCUMENT: the vocabulary docs/14 §2 names, the Go
// types that carry it, and the canonical rendering that is pinned by the
// golden fixtures (docs/24 §6 lists "Search query plan" among the artifacts
// that must have golden fixtures).
//
// # The vocabulary is the spec's, one property per spec item
//
// "自然语言问题先解析为：intent、target object、property、condition/scope、
// evidence preference、network scope、visibility、ranking constraints"
// (docs/14 §2) — eight items, and the document has exactly eight top-level
// properties named after them (plus plan_version), so "which field carries
// spec item N" is answerable by reading the spec and this struct side by
// side instead of by inference.
//
// The VALUE vocabularies are closed wherever the platform already has one,
// and copying is deliberate: target_object.entity_types is the search
// projection's own entity vocabulary (internal/search), so a plan cannot name
// a thing the index does not hold; evidence_preference.types is
// domain.CanonicalEvidenceTypes (docs/10 §3), so the plan cannot invent an
// evidence kind; ranking_constraints.order_by is docs/14 §3's own list of
// ranking criteria, word for word. Two of those lists exclude by
// construction what the spec forbids: no popularity/star/prestige can be
// asked for, and no score of any kind exists to ask for (docs/14 §3,
// CLAUDE.md §9.13: no Truth Score, no Research Score).
//
// # The plan carries filters and preferences, never entities
//
// No field of this document can hold an entity identity. That is the
// structural half of docs/54's scenario #7 ("Search LLM 引用未授权 entity
// id") and of docs/22 §8 ("Answer Generator 只可引用 planner/retrieval 返回
// 的 entity ids/version"): the answer may cite what RETRIEVAL returned, under
// the searcher's scope, and a plan that named an entity itself could only be
// an entity named from outside that scope. There is therefore no
// entity_ids/candidates/sources field to validate — the schema's
// additionalProperties:false refuses one by name, and the identifier guard in
// planner.go refuses one smuggled into a free-text value.
//
// Making the model emit candidate ids and filtering them afterwards is the
// mistake this shape exists to prevent (docs/21 §9: authorization is not
// "hide it after the query").

// PlanVersion is the plan document's version (docs/21 §8: a stored artifact
// pins the schema version it was written under, so a plan saved by T0906
// stays interpretable after the vocabulary moves).
const PlanVersion = "1"

// Status says how a Plan came to be.
type Status string

const (
	// StatusPlanned: the provider's document validated and is carried in
	// Plan.Document.
	StatusPlanned Status = "planned"
	// StatusFallback: the provider failed, timed out, or produced a
	// document this package refused. Plan.Document is nil and Plan.Query is
	// the caller's question, unchanged, so the caller still has everything
	// the structured path needs (docs/27 §SLO: "超时提供 structured results
	// fallback"; docs/32 risk register: "fallback structured results").
	StatusFallback Status = "fallback"
)

// Reason names why a Plan fell back. It is a closed set so the fallback rate
// can be counted per cause and a new cause is a deliberate change
// (docs/26 §3 lists "LLM planner failures" among the metrics).
type Reason string

const (
	// ReasonProviderError: the provider answered with an error.
	ReasonProviderError Reason = "provider_error"
	// ReasonTimeout: the provider did not answer within the planner's
	// budget.
	ReasonTimeout Reason = "provider_timeout"
	// ReasonInvalidPlan: the document was not valid JSON, or it violated
	// the plan schema (plan_schema.go) — including an unknown field.
	ReasonInvalidPlan Reason = "invalid_plan"
	// ReasonIdentifier: the document was schema-valid but carried an
	// entity-identifier-shaped token in one of its values.
	ReasonIdentifier Reason = "identifier_in_plan"
)

// The intent vocabulary: the facets of the answer page the user is asking
// for (docs/14 §4 — 直接回答、comparison、conditions、origin assessment).
const (
	IntentAnswer           = "answer"
	IntentCompare          = "compare"
	IntentConditions       = "conditions"
	IntentOriginAssessment = "origin_assessment"
)

// The evidence_preference.prefer vocabulary: the attributes docs/14 §3 ranks
// on, minus the ones naming the query itself (query/scope match, freshness)
// which are ranking_constraints' business.
const (
	EvidencePreferReviewed                = "reviewed"
	EvidencePreferIndependentlyReproduced = "independently_reproduced"
	EvidencePreferContradictory           = "contradictory"
)

// The comparator operand vocabulary: the comparison operators a condition may
// assert. Values stay strings — the plan states the condition as it was
// asked, and unit conversion, numeric parsing and cross-property comparison
// are the retrieval layer's, not the planner's (docs/14 §2 puts "structured
// filters + FTS + ..." AFTER planning).
const (
	ComparatorLT  = "lt"
	ComparatorLTE = "lte"
	ComparatorEQ  = "eq"
	ComparatorGTE = "gte"
	ComparatorGT  = "gt"
)

// The network_scope vocabulary (docs/14 §1: the platform network's
// public/accessible content, plus external references formally taken into a
// project — never a whole-internet crawler).
const (
	NetworkScopePlatform     = "platform"
	NetworkScopeWithExternal = "platform_and_external"
)

// The visibility vocabulary. These are the two classes docs/14 §1 names
// ("public/accessible content") and they are a NARROWING HINT only:
//
//   - NetworkVisibilityAccessible means "whatever the searcher's scope
//     already allows" — it never widens anything, and in particular it does
//     not mean "the network's", which no search may read without a scope.
//   - NetworkVisibilityPublic narrows that to rows the read query returns to
//     anybody (search.sql: `visibility = 'public'`).
//
// The retrieval layer intersects this with the resolved scope; the plan is
// the only place the two ever meet, and the intersection can only shrink.
const (
	VisibilityAccessible = "accessible"
	VisibilityPublic     = "public"
)

// The ranking_constraints.order_by vocabulary: docs/14 §3's ranking criteria,
// word for word ("主要考虑 query/scope match、evidence profile、review state、
// independent reproduction、contradictory evidence、version/freshness"). The
// sentence's own prohibition is why the list is closed: popularity, stars and
// organization prestige are not in it, so a plan cannot ask for them, and
// nothing here is a score — order_by selects one of the criteria the ranking
// layer already ranks on.
const (
	RankQueryScopeMatch         = "query_scope_match"
	RankEvidenceProfile         = "evidence_profile"
	RankReviewState             = "review_state"
	RankIndependentReproduction = "independent_reproduction"
	RankContradictoryEvidence   = "contradictory_evidence"
	RankVersionFreshness        = "version_freshness"
)

// Plan is what the planner returns for one question, and it is deliberately
// two things in one value: the document (when there is one) and the story of
// how it was obtained. A caller decides what to run from Status alone, and
// never has to interpret a half-filled document.
type Plan struct {
	// Status is StatusPlanned or StatusFallback.
	Status Status
	// Reason is set iff Status is StatusFallback.
	Reason Reason
	// Query is the caller's question, verbatim, in both statuses. It is what
	// the structured path runs on when there is no plan, and it is NOT part
	// of the document: the document is what a model wrote, this is what the
	// user typed.
	Query string
	// Document is the validated plan, or nil in the fallback case.
	Document *Document
}

// Planned reports whether the plan carries a validated document.
func (p Plan) Planned() bool { return p.Status == StatusPlanned && p.Document != nil }

// Document is the plan vocabulary as a value. Its field order is the spec's
// order (docs/14 §2) and its json tags are the wire vocabulary the golden
// fixtures pin.
type Document struct {
	// PlanVersion pins the vocabulary version (PlanVersion).
	PlanVersion string `json:"plan_version"`
	// Intent is what the asker wants (Intent*).
	Intent string `json:"intent"`
	// TargetObject is the spec's "target object": which entity kinds the
	// question is about.
	TargetObject TargetObject `json:"target_object"`
	// Property is the spec's "property": the properties the question names,
	// e.g. a materials property such as CO2 uptake. The vocabulary is OPEN
	// on purpose — properties are project-defined (project schema profiles,
	// internal/application/schemaprofiles) — so the field is bounded text,
	// not an enum.
	Property *PropertyFilter `json:"property,omitempty"`
	// ConditionScope is the spec's "condition/scope": the conditions the
	// question attaches to those properties.
	ConditionScope *ConditionScope `json:"condition_scope,omitempty"`
	// EvidencePreference is the spec's "evidence preference".
	EvidencePreference *EvidencePreference `json:"evidence_preference,omitempty"`
	// NetworkScope is the spec's "network scope" (NetworkScope*).
	NetworkScope string `json:"network_scope"`
	// Visibility is the spec's "visibility" (Visibility*): a narrowing hint,
	// never a grant.
	Visibility string `json:"visibility"`
	// RankingConstraints is the spec's "ranking constraints".
	RankingConstraints *RankingConstraints `json:"ranking_constraints,omitempty"`
}

// TargetObject names the entity kinds the question addresses. At least one is
// required: "no target" is not "any target", and a document that named none
// would read as a search of everything.
type TargetObject struct {
	// EntityTypes is a non-empty subset of the search projection's entity
	// vocabulary (search.EntityAsset, search.EntityKnowledge,
	// search.EntityRelease, search.EntityState) — the kinds the index
	// actually holds.
	EntityTypes []string `json:"entity_types"`
}

// PropertyFilter is the property half of the question.
type PropertyFilter struct {
	// Names are the property names the question asked about, in the order it
	// named them.
	Names []string `json:"names"`
}

// ConditionScope is the condition half of the question: the qualifiers the
// answer must hold under.
type ConditionScope struct {
	// Text is the condition as the question phrased it, kept verbatim when
	// the comparator vocabulary below cannot express it. Nothing is lost: a
	// caller that cannot act on Text still knows what the question said.
	Text string `json:"text,omitempty"`
	// Comparators are the conditions that ARE expressible as a property
	// compared against a value.
	Comparators []Comparator `json:"comparators,omitempty"`
}

// Comparator is one condition: a property, an operator and a value.
type Comparator struct {
	// Property is the property the condition constrains.
	Property string `json:"property"`
	// Op is the comparison (Comparator*).
	Op string `json:"op"`
	// Value is the value compared, as text: the plan states what was asked,
	// and parsing is the retrieval layer's business.
	Value string `json:"value"`
	// Unit is the value's unit when the question stated one (K, mmol/g, ...).
	Unit string `json:"unit,omitempty"`
}

// EvidencePreference is what the asker wants the evidence to be like. Both
// halves are optional, but at least one is required when the field is
// present: an empty preference object states nothing.
type EvidencePreference struct {
	// Types are the evidence kinds preferred, from
	// domain.CanonicalEvidenceTypes (docs/10 §3).
	Types []string `json:"types,omitempty"`
	// Prefer are the attributes docs/14 §3 ranks on
	// (EvidencePrefer*).
	Prefer []string `json:"prefer,omitempty"`
}

// RankingConstraints is the ranking half of the question, constrained to the
// criteria docs/14 §3 names. It holds no weights and no scores: ranking
// weights would be a research-quality score by another name (CLAUDE.md
// §9.13), and the spec fixes the criteria rather than letting a caller tune
// them.
type RankingConstraints struct {
	// OrderBy is the criteria to rank by, most significant first.
	OrderBy []string `json:"order_by"`
}

// planEnvelope is the canonical rendering of a Plan: the lifecycle fields
// first, then the document (or nothing, when the plan fell back).
type planEnvelope struct {
	Status   Status    `json:"status"`
	Reason   Reason    `json:"reason,omitempty"`
	Query    string    `json:"query"`
	Document *Document `json:"document,omitempty"`
}

// CanonicalJSON renders the plan as the bytes a consumer stores or compares:
// the envelope's fields in a fixed order, the document's fields in the spec's
// order, HTML escaping off (a plan about "CO2 -> CH4" must not be rewritten
// into >), two-space indented and newline-terminated.
//
// The golden fixtures (tests/searchplan) pin these bytes, so a change to a
// field name, an enum value or the rendering fails there and needs a
// deliberate fixture update (docs/24 §6).
func (p Plan) CanonicalJSON() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	// The conversion is deliberate and not a shorthand: planEnvelope is the
	// same four fields by name, so adding a field to Plan without teaching the
	// envelope about it is a compile error rather than a silently dropped
	// value.
	if err := enc.Encode(planEnvelope(p)); err != nil {
		return nil, fmt.Errorf("planner: render plan: %w", err)
	}
	return buf.Bytes(), nil
}
