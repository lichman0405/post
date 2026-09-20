package ranking

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/lichman0405/post/internal/search/retrieval"
)

// Assessment is one factor's outcome on one candidate: the level it reached,
// where that level sits on the factor's own ladder, and the sentence a reader
// is owed.
//
// It is the whole of what "透明 explanation fields" (T0905's requirement)
// means here. A ranking that returned only an order would be unfalsifiable —
// a reader could not tell a bug from a judgement — so every position in the
// result is the lexicographic product of a list of Assessments, and the
// Assessments are the only inputs to that product. There is nothing else
// behind the order to disagree with.
type Assessment struct {
	// Factor is one of the six names in Factors.
	Factor string `json:"factor"`
	// Level is the factor's ladder position, named (see factor.go).
	Level string `json:"level"`
	// LevelRank is Level's position on its factor's ladder (0 = best). It is
	// the comparison key, stated rather than implied, so a consumer can sort
	// or group without re-deriving the ladder.
	LevelRank int `json:"level_rank"`
	// Reason states the facts the level was read from, in one sentence. It
	// names the counts and the sources it read and no more: a reason that
	// editorialised ("strong evidence") would be a second, unreviewable
	// judgement layered on the level.
	Reason string `json:"reason"`
	// Facts is the machine-readable half of Reason: the counts the level was
	// computed from, keyed by the column they came from
	// (internal/persistence/queries/search.sql, SearchRankingFacts). It is
	// present so that a caller checking a level against its own view of the
	// database never has to parse Reason.
	Facts map[string]int `json:"facts,omitempty"`
}

// Ranked is one candidate at its position, with the reasons for it.
type Ranked struct {
	// Rank is the position in the result, 1-based. It is a POSITION and not
	// a quality: there is deliberately no score field beside it, because
	// any number that orders candidates reads as an assessment of the
	// underlying science (CLAUDE.md §9.13, docs/10 §8).
	Rank int `json:"rank"`
	// Ref is the candidate's citation identity, unchanged from retrieval
	// (search.EntityRef plus the version pin) — re-ranking never renames
	// what is cited.
	Ref string `json:"ref"`
	// Kind is retrieval.KindDocument or retrieval.KindObjectVersion.
	Kind string `json:"kind"`
	// EntityType is the projection's entity type for a document.
	EntityType string `json:"entity_type,omitempty"`
	// ObjectType is scientific_objects.object_type when it is known.
	ObjectType string `json:"object_type,omitempty"`
	// Title is the candidate's title.
	Title string `json:"title,omitempty"`
	// Version is the pinned version label ("" when the entity has none).
	Version string `json:"version,omitempty"`
	// ObjectVersionID is the scientific object version the candidate
	// resolves to, when it resolves to one. It is the identity the factors
	// were read for, so it is also the key a reader uses to check them.
	ObjectVersionID string `json:"object_version_id,omitempty"`
	// Labels are docs/10 §8's descriptive labels, sorted.
	Labels []string `json:"labels"`
	// Factors are the six Assessments in priority order: the first is the
	// strongest reason for this position.
	Factors []Assessment `json:"factors"`

	// Candidate is the retrieval candidate this position was computed for,
	// carried whole so the answer layer (T0906) cites what it was given
	// rather than reconstructing it. It is deliberately NOT part of the
	// canonical rendering: the ranking's contract is the order and its
	// reasons, and pinning the recall signals and the fusion score in a
	// ranking fixture would make every fusion change a ranking-fixture diff.
	Candidate retrieval.Candidate `json:"-"`
}

// Result is one ranking: the question, the factor vocabulary it was ordered
// by, and the candidates in order.
type Result struct {
	// Query is the question the ranking was run for (the retrieval's own).
	Query string `json:"query"`
	// Factors is the priority order the result was ordered by, so a reader
	// of a stored result knows what the positions mean without knowing this
	// package's current revision.
	Factors []string `json:"factors"`
	// Ranked are the candidates, best first.
	Ranked []Ranked `json:"ranked"`
}

// resultEnvelope is Result's canonical rendering shape. It is a distinct type
// for the reason planner.planEnvelope is: a field added to Result without
// being taught to the envelope is a compile error rather than a value that
// silently never reaches a fixture.
type resultEnvelope struct {
	Query   string   `json:"query"`
	Factors []string `json:"factors"`
	Ranked  []Ranked `json:"ranked"`
}

// CanonicalJSON renders the result as the bytes a consumer stores or
// compares: a fixed field order, HTML escaping off (a question about
// "CO2 -> CH4" must not be rewritten into "CO2 -\u003e CH4"), two-space indented
// and newline-terminated.
//
// tests/ranking's golden fixtures pin these bytes, so a change to a field
// name, a level name or a reason's wording fails there and needs a
// deliberate fixture update (docs/24 §6 names "Search query plan/answer
// citation" as fixture-bearing; the ranking is the ordering half of that
// interface).
func (r Result) CanonicalJSON() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	// The conversion is deliberate and not a shorthand, for the same reason
	// planner.CanonicalJSON gives: resultEnvelope is the same three fields by
	// name, so adding a field to Result without teaching the envelope about
	// it is a compile error rather than a silently dropped value.
	if err := enc.Encode(resultEnvelope(r)); err != nil {
		return nil, fmt.Errorf("ranking: render result: %w", err)
	}
	return buf.Bytes(), nil
}
