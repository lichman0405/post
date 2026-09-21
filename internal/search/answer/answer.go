package answer

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/lichman0405/post/internal/search/ranking"
)

// AnswerVersion is the answer document's version. A stored answer pins the
// vocabulary it was written under, the way a stored plan pins
// planner.PlanVersion (docs/21 §8): docs/22 §8 asks the server to save the
// answer, and a saved document that silently means something else after a
// vocabulary change is worse than one that names its revision.
const AnswerVersion = "1"

// Status says how an Answer came to be.
type Status string

const (
	// StatusAnswered: a provider's document survived the schema and the
	// grounding guard, and its summary is carried in Answer.Summary.
	StatusAnswered Status = "answered"
	// StatusFallback: no summary was produced. The answer is the
	// structured result — the ranked sources, the platform's limitations
	// and conflicts, and the reason no summary was written.
	StatusFallback Status = "fallback"
)

// Reason names why an Answer fell back. It is a closed set, so the fallback
// rate is countable per cause (docs/26 §3 lists LLM failures among the
// metrics; this package logs each one) and so a reader of a stored answer
// knows what happened without knowing this package's revision.
//
// The four provider-shaped reasons are deliberately the same situations
// planner.Reason names for planning, and two of them are spelled the same
// way: one cause, one word, across the two halves of a search.
type Reason string

const (
	// ReasonNoProvider: no provider is configured. This is the state of a
	// deployment that has not answered "may a question and its retrieved
	// titles go to a third-party service" (doc.go), and it is not an error:
	// the structured result is a complete answer to the search.
	ReasonNoProvider Reason = "no_provider"
	// ReasonNoSources: the search returned no candidate, so there is
	// nothing a summary could cite. The provider is NOT called for this
	// case — a model asked to answer with an empty citation vocabulary can
	// only invent one.
	ReasonNoSources Reason = "no_sources"
	// ReasonProviderError: the provider answered with an error.
	ReasonProviderError Reason = "provider_error"
	// ReasonTimeout: the provider did not answer within the generator's
	// deadline.
	ReasonTimeout Reason = "provider_timeout"
	// ReasonInvalidAnswer: the document was not valid JSON, or it violated
	// the schema in schema.go.
	ReasonInvalidAnswer Reason = "invalid_answer"
	// ReasonUngroundedCitation: the document was schema-valid but cited, or
	// mentioned, an entity retrieval did not return (grounding.go). This is
	// docs/54 #7 ("Search LLM 引用未授权 entity id") happening, caught.
	ReasonUngroundedCitation Reason = "ungrounded_citation"
)

// originPlatform is the one value of Statement.Origin this package produces,
// and the distinction it draws is the difference between what the platform
// read and what a model wrote: every Statement the platform derives is a
// restatement of a fact in its own tables (a factor level, a signal that did
// not run), while the summary is a view (docs/14 §4).
//
// A model-authored statement would be spelled "answer". Nothing produces one
// today — the provider authors the summary and nothing else, and the summary
// is Summary rather than a Statement — so there is no constant for it here:
// an unused second value would be vocabulary the package does not have.
const originPlatform = "platform"

// Statement is one sentence an answer says about its own evidence: a
// limitation, or a conflict.
//
// Refs names the sources the statement is about, and every ref in it is a
// source of the same answer, by construction: the platform derives the
// statements FROM the sources (derive.go), so a statement cannot point at an
// entity the answer does not carry. An empty list is a statement about the
// search rather than about any one source — "the vector signal did not run"
// is about the whole query.
type Statement struct {
	// Text is the sentence, in the platform's own words.
	Text string `json:"text"`
	// Refs are the sources it is about, in the sources' rank order.
	Refs []string `json:"refs,omitempty"`
	// Origin is originPlatform; "answer" is reserved for a model-authored
	// statement, which nothing produces yet.
	Origin string `json:"origin"`
}

// Source is one entity the answer may cite — and, at the same time, one
// underlying result.
//
// # Why sources and results are one list
//
// docs/14 §4 asks an answer page for both "source objects" and "underlying
// results", and this package renders them as ONE list because they are the
// same rows: the rank order, the labels and the factor explanations ARE the
// underlying result, and every source is a candidate retrieval returned.
// Two lists would be one list twice — and, worse, two lists that can
// disagree about which entities a search found. Cited marks the subset the
// summary actually leans on, so a consumer that wants "the sources the
// answer used" filters on it and the two views cannot drift apart.
type Source struct {
	// Rank is the source's position in the ranking, 1-based. Like
	// ranking.Ranked.Rank it is a POSITION and not a quality: no number
	// here assesses the science (CLAUDE.md §9.13, docs/10 §8).
	Rank int `json:"rank"`
	// Ref is the citation identity, verbatim from retrieval (search.EntityRef
	// plus the version pin) — never rewritten by this layer, because it is
	// what the summary's citations are matched against.
	Ref string `json:"ref"`
	// Kind is retrieval.KindDocument or retrieval.KindObjectVersion.
	Kind string `json:"kind"`
	// EntityType is the projection's entity type for a document.
	EntityType string `json:"entity_type,omitempty"`
	// ObjectType is scientific_objects.object_type when it is known.
	ObjectType string `json:"object_type,omitempty"`
	// Title is the entity's title.
	Title string `json:"title,omitempty"`
	// Version is the pinned version label ("" when the entity has none).
	Version string `json:"version,omitempty"`
	// Href is where a click on this source lands, or "" when the platform
	// has no address for this kind of entity (locator.go). It is never a
	// guess: a fabrication would be a link to something the answer did not
	// find, which is the same defect as a fabricated citation.
	Href string `json:"href,omitempty"`
	// ProjectID is the owning project's uuid text. It travels with the
	// source even when Href is empty: it is how a client scoped to the
	// project addresses an object version this API has no route for.
	ProjectID string `json:"project_id,omitempty"`
	// Labels are docs/10 §8's descriptive labels, sorted (the ranking's
	// own list, unchanged).
	Labels []string `json:"labels"`
	// Factors are the ranking's six Assessments in priority order: the
	// reasons this source sits where it does, carried through so the answer
	// never shows an order it cannot explain.
	Factors []ranking.Assessment `json:"factors"`
	// Cited reports whether the answer's citations name this source. It is
	// false for every source of a fallback answer.
	Cited bool `json:"cited"`
}

// Answer is one evidence-backed answer: what the network holds about a
// question, with the sources it may be checked against.
type Answer struct {
	// Version is AnswerVersion.
	Version string `json:"answer_version"`
	// Status is StatusAnswered or StatusFallback.
	Status Status `json:"status"`
	// Reason is set iff Status is StatusFallback.
	Reason Reason `json:"reason,omitempty"`
	// Query is the question the answer is for (the retrieval's own).
	Query string `json:"query"`
	// AnswerView reports whether Summary was written by a model. docs/14 §4
	// requires an agent summary to be labelled as a View and to create no
	// new claims; a boolean says so in the document itself rather than in a
	// convention a client has to know. It is false for every fallback, which
	// carries no summary at all.
	AnswerView bool `json:"answer_view"`
	// Summary is the direct answer, in the model's words. It is EMPTY for a
	// fallback: a caller decides what to render from Status alone and never
	// has to ask whether an empty string was an answer.
	Summary string `json:"summary"`
	// Citations are the refs the summary relies on, each one a Source.Ref of
	// this answer (grounding.go checked it before the summary was kept), in
	// the order the provider listed them. Empty for a fallback.
	Citations []string `json:"citations"`
	// Limitations are what limits this answer: the search signals that did
	// not run, the sources nothing has been asserted about, the sources
	// whose facts the platform could not read. Never empty — a fallback's
	// first limitation is that it is a fallback.
	Limitations []Statement `json:"limitations"`
	// Conflicts are the recorded contradictions touching these sources, one
	// statement per contested source, plus — when every source's conflict
	// facts were readable — the single statement that nothing in the result
	// is contested.
	Conflicts []Statement `json:"conflicts"`
	// Sources are the candidates the search returned, in rank order.
	Sources []Source `json:"sources"`
}

// answerEnvelope is Answer's canonical rendering shape.
//
// It is a distinct type for the reason planner.planEnvelope and
// ranking.resultEnvelope are: a field added to Answer without being taught
// to the envelope is a compile error, not a value that silently never
// reaches a fixture or a client.
type answerEnvelope struct {
	Version     string      `json:"answer_version"`
	Status      Status      `json:"status"`
	Reason      Reason      `json:"reason,omitempty"`
	Query       string      `json:"query"`
	AnswerView  bool        `json:"answer_view"`
	Summary     string      `json:"summary"`
	Citations   []string    `json:"citations"`
	Limitations []Statement `json:"limitations"`
	Conflicts   []Statement `json:"conflicts"`
	Sources     []Source    `json:"sources"`
}

// CanonicalJSON renders the answer as the bytes a consumer stores or
// compares: a fixed field order, HTML escaping off (a question about
// "CO2 -> CH4" or a title containing "<" must not be rewritten into escapes),
// two-space indented and newline-terminated.
//
// tests/answer pins these bytes, so a change to a field name, to a reason
// name or to a derived statement's wording fails there and needs a
// deliberate fixture update. docs/24 §6 names "Search query plan/answer
// citation" as fixture-bearing, and this is the citation half of that
// interface.
func (a Answer) CanonicalJSON() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	// The conversion is deliberate and not a shorthand: answerEnvelope is
	// Answer's own fields by name, in Answer's order, so adding a field to
	// Answer without teaching the envelope about it is a compile error rather
	// than a silently dropped value. It is the form
	// planner.Plan.CanonicalJSON and ranking.Result.CanonicalJSON use.
	if err := enc.Encode(answerEnvelope(a)); err != nil {
		return nil, fmt.Errorf("answer: render answer: %w", err)
	}
	return buf.Bytes(), nil
}
