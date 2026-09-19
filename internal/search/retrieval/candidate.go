package retrieval

import (
	"encoding/json"
	"strings"

	"github.com/lichman0405/post/internal/search"
)

// Candidate kinds. A candidate from a document is a row of the projection; a
// candidate from the graph is a scientific object version. The distinction is
// reported because the two are authorized differently (a row by its own
// visibility, a version by its project's membership in the scope) and because
// an answer layer must know whether it is citing a published document or an
// object inside a project.
const (
	// KindDocument: the candidate is a search_documents row.
	KindDocument = "document"
	// KindObjectVersion: the candidate is a scientific_object_versions row
	// reached by traversal.
	KindObjectVersion = "object_version"
)

// Hop is one traversal step that reached a candidate: what was walked, from
// where, in which direction, and how far out. It exists so a result can
// explain WHY an entity the question never mentioned is in the answer
// (docs/14 §4's "Research Map" and docs/24's determinism expectations both
// need the path, not just the endpoint).
type Hop struct {
	// RelationType is the canonical relation type the hop crossed.
	RelationType string `json:"relation_type"`
	// Direction is "out" (the edge leaves the frontier) or "in" (it
	// arrives at it).
	Direction string `json:"direction"`
	// FromRef is the candidate the hop left from, rendered the same way
	// Candidate.Ref is.
	FromRef string `json:"from_ref"`
	// Depth is 1 for a node adjacent to a seed.
	Depth int `json:"depth"`
}

// Direction values.
const (
	DirectionOut = "out"
	DirectionIn  = "in"
)

// SignalHit says that one signal recalled a candidate, at which position in
// that signal's own ranking, with that signal's own score.
//
// Rank is 1-based and is the fusion input; Score is explanation only. The
// two are both reported because they mean different things: a ts_rank of
// 0.06 and a cosine similarity of 0.31 are not the same kind of number, and
// nothing in this package treats them as though they were (see fuse).
type SignalHit struct {
	Signal string  `json:"signal"`
	Rank   int     `json:"rank"`
	Score  float64 `json:"score"`
}

// Candidate is one recalled, version-pinned entity: what a citation may
// name.
type Candidate struct {
	// Ref is the citation identity: "kind:identity" when the entity carries
	// no version label, "kind:identity@version" when it does
	// (search.EntityRef plus search.PinnedVersion, the shape
	// examples/search-answer.example.json renders).
	Ref string `json:"ref"`
	// Kind is KindDocument or KindObjectVersion.
	Kind string `json:"kind"`
	// EntityType is the projection's entity type for a document; empty for
	// an object version, whose kind is ObjectType instead.
	EntityType string `json:"entity_type,omitempty"`
	// Identity is the entity's own identity: a pid, a row id, or an object
	// version's uuid.
	Identity string `json:"identity"`
	// Version is the pinned version label ("" when the entity has none —
	// see search.PinnedVersion).
	Version string `json:"version,omitempty"`
	// Title is the entity's title.
	Title string `json:"title,omitempty"`
	// Visibility is the projected row's own visibility for a document, and
	// "" for an object version: a scientific object version has no
	// visibility column of its own — what admits it is the actor's
	// membership in its project, which is what the hop query enforced.
	Visibility string `json:"visibility,omitempty"`
	// ProjectID is the owning project's uuid text.
	ProjectID string `json:"project_id,omitempty"`
	// ObjectType is scientific_objects.object_type when it is known:
	// "claim", "material", "finding", ... For a published document it is
	// read from the facet the projection wrote; for a graph node it is the
	// node's own column.
	ObjectType string `json:"object_type,omitempty"`
	// ObjectID and ObjectVersionID are set when the candidate resolves to
	// a scientific object version: the graph node itself, or (for a
	// published knowledge document) the version the publication pins.
	ObjectID        string `json:"object_id,omitempty"`
	ObjectVersionID string `json:"object_version_id,omitempty"`
	// VersionNo is the pinned version's ordinal (0 when unknown).
	VersionNo int `json:"version_no,omitempty"`
	// Hops are the traversal steps that reached this candidate (empty for a
	// document recalled by a text or facet signal).
	Hops []Hop `json:"hops,omitempty"`
	// Signals are the signals that recalled it, most significant first
	// (highest fusion contribution).
	Signals []SignalHit `json:"signals"`
	// Score is the fusion score: a RECALL ordering device, not a measure of
	// scientific quality, evidence strength or relevance to a research
	// question. docs/14 §3's ranking is T0905's, and CLAUDE.md §9.13
	// forbids a research score outright — nothing downstream may present
	// this number as one.
	Score float64 `json:"score"`
}

// Pinned reports whether the candidate names a version, which is the
// property ADR-010 needs from retrieval: every candidate an answer may cite
// resolves to one version of one entity.
//
// A document of an entity type with no version label (a project state) is
// pinned by its own immutable identity — see search.PinnedByIdentity, which
// is where that decision lives because it is a property of the projection's
// vocabulary and not of this package.
func (c Candidate) Pinned() bool {
	if c.Version != "" || c.ObjectVersionID != "" {
		return true
	}
	return search.PinnedByIdentity(c.EntityType) && c.Identity != ""
}

// renderRef builds the citation identity. A version label is part of it; an
// absent one is omitted rather than rendered as an empty "@".
func renderRef(ref, version string) string {
	if version == "" {
		return ref
	}
	return ref + "@" + version
}

// documentCandidate turns one projection hit into a candidate.
//
// The version label and the object type come from the row's own facets,
// read through the projection's own accessors (search.PinnedVersion) rather
// than by this package knowing which key means what — see search/pin.go for
// why that table lives with the writer.
func documentCandidate(hit DocumentHit) Candidate {
	ref := hit.Ref
	if ref == "" {
		ref = search.EntityRef(hit.EntityType, "")
	}
	version := search.PinnedVersion(hit.EntityType, hit.Structured)
	identity, _ := splitEntityRef(ref)
	if identity == "" {
		// A row whose entity_ref is not "kind:identity" cannot be a
		// candidate: it has no identity to cite. The projection writes
		// every ref through search.EntityRef, so this is unreachable in
		// production and is a floor for a hand-inserted row.
		identity = ref
	}
	return Candidate{
		Ref:        renderRef(ref, version),
		Kind:       KindDocument,
		EntityType: hit.EntityType,
		Identity:   identity,
		Version:    version,
		Title:      hit.Title,
		Visibility: hit.Visibility,
		ProjectID:  hit.ProjectID,
		ObjectType: facetString(hit.Structured, "object_type"),
	}
}

// splitEntityRef splits "kind:identity" at the FIRST colon, mirroring
// search.EntityRef: an identity that itself contains a colon would otherwise
// be split at the wrong one.
func splitEntityRef(ref string) (identity string, ok bool) {
	idx := strings.IndexByte(ref, ':')
	if idx < 0 || idx == len(ref)-1 {
		return "", false
	}
	return ref[idx+1:], true
}

// facetString reads one string facet out of a row's structured object,
// returning "" for anything else. It is the same fail-closed read
// search.PinnedVersion makes, for facets this package does not have a table
// for: object_type is reported when present and is simply absent otherwise.
func facetString(structured []byte, key string) string {
	if len(structured) == 0 {
		return ""
	}
	var facets map[string]any
	if err := json.Unmarshal(structured, &facets); err != nil {
		return ""
	}
	s, _ := facets[key].(string)
	return s
}
