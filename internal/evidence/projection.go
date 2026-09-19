package evidence

import (
	"encoding/json"

	"github.com/lichman0405/post/internal/domain"
)

// The evidence-graph read model (T0506): the assertions the project surface
// renders, GROUPED BY THE TARGET OBJECT VERSION each one pins. The word the
// requirement uses is grouping, not union — one group per pinned target, and
// nothing is ever merged across targets, across stances, or into a number.
//
// Three rules live in this file and nowhere else:
//
//  1. Grouping. A group exists per pinned target version (GroupAll), and a
//     row is filed under the target it pins. Two assertions on the SAME pair
//     — one supports, one contradicts — are two rows in two buckets; the
//     storage layer deliberately does not constrain that pair to be unique
//     (infra/migrations/00058 lines 38-46: "A unique constraint on the pair
//     would collapse the two stances into one row and force a winner —
//     exactly the net-position arithmetic the domain forbids").
//  2. Stance labelling is the domain's transparent rule, applied once, here:
//     domain.RelationStance maps a relation to supporting / contesting /
//     neutral, and answers ok=false for anything else. That false is
//     FAIL-CLOSED, not an omission: a relation the rule does not know is
//     rendered with NO stance label (Assertion.Stance is omitted from the
//     wire) and filed in the Unlabeled bucket. It is never guessed into
//     neutral — neutral belongs to contextualizes alone.
//  3. No arithmetic. No count, ratio, total, weight or net position is
//     computed here or rendered by this model: docs/10 §4 「V1 不自动赋数值
//     权重」 and CLAUDE.md §9.13 「不生成 Truth Score / Research Score」. The
//     only numbers on the wire are identifiers' own (a version number, an
//     instant), which is why the read model's tests scan the marshalled
//     document for aggregate-shaped keys rather than trusting this comment.

// Assertion is one evidence assertion as the evidence-graph read renders it:
// the stored row's own facts. The store fills everything except Stance,
// which GroupAll derives from Relation (one rule, one place, so a row can
// never be bucketed "supporting" while carrying some other label).
type Assertion struct {
	ID string `json:"id"`
	// Relation is one of docs/10 §4's nine evidence relations, as stored.
	Relation string `json:"relation"`
	// Stance is the label the domain rule derives from Relation. It is
	// OMITTED (not empty-string, not null) when the rule has no answer for
	// the relation: a client that sees no stance must read "the platform
	// does not place this assertion on either side", never "neutral".
	Stance string `json:"stance,omitempty"`
	// EvidenceType is the source's kind (docs/10 §3, the schema's
	// evidence_type enum) — the one REAL source axis this build can fill.
	EvidenceType string `json:"evidence_type"`
	// Origin is the origin/external placeholder of docs/10 §3. It is
	// ALWAYS null in this build, on purpose: the axis is the network read's
	// classification (domain.ClassifyEvidenceNetwork, T0806), not this
	// projection's, and migration 00091 records that the read side
	// deliberately does not consult the stored evidence_origin column ("a
	// declaration that could be edited must not be able to move a row
	// between buckets"). A value invented to fill the field would be a
	// fabricated source claim about the evidence, which is worse than an
	// empty placeholder. The field is present so the client can see the
	// axis exists and is unfilled.
	Origin *string `json:"origin"`
	// Directness and InferenceNature are the schema's declarations (empty
	// string means the column holds none).
	Directness      string `json:"directness"`
	InferenceNature string `json:"inference_nature"`
	// Scope bounds where the assertion holds; it is passed through raw.
	Scope json.RawMessage `json:"scope"`
	// ReasoningNote is the author's explanation, as stored ("" when NULL).
	ReasoningNote string `json:"reasoning_note"`
	// ReviewState is the reviewer's position on this assertion. A rejected
	// assertion is shown as rejected, never laundered (invariant 8).
	ReviewState string `json:"review_state"`
	// TargetObjectVersionID and EvidenceObjectVersionID are the two version
	// pins (docs/10 §3): the assertion answers for the versions it saw.
	TargetObjectVersionID   string `json:"target_object_version_id"`
	EvidenceObjectVersionID string `json:"evidence_object_version_id"`
	// CreatedAt is the assertion's creation instant, formatted by the
	// adapter (knowledgepublish.FormatInstant — one instant spelling on
	// this API).
	CreatedAt string `json:"created_at"`
}

// Target names the exact object version one group is about.
type Target struct {
	// ObjectVersionID is the pin the group is keyed by.
	ObjectVersionID string `json:"object_version_id"`
	// VersionNo is the position of that version in its object's log,
	// resolved by the caller's version read. It is omitted when the version
	// log did not resolve the pin — the read then states the pin it has and
	// invents no number (0 would be a false claim: versions are 1-based).
	VersionNo *int `json:"version_no,omitempty"`
	// Title is the pinned version's own title, from the same version read.
	Title string `json:"title"`
}

// TargetGroup is one pinned target version's evidence. The four arrays are
// always present and never null (an empty array is an answer; a missing one
// would read as "nothing to say" only by accident).
//
// Supporting and Contesting are SEPARATE arrays because the alternative —
// one list, or one netted list — is the thing the design forbids: docs/10 §7
// (原发布者不能删除外部反证) and 00058's header both exist so that a
// contradiction stays visible next to the support it contradicts.
type TargetGroup struct {
	Target     Target      `json:"target"`
	Supporting []Assertion `json:"supporting"`
	Contesting []Assertion `json:"contesting"`
	Neutral    []Assertion `json:"neutral"`
	// Unlabeled is NOT a fourth stance and does not mean "no evidence". It
	// holds the assertions whose relation domain.RelationStance does not
	// map (ok=false), including a relation the canonical set does not name.
	// They are rendered as stored, with no stance label, never folded into
	// one of the three buckets above.
	Unlabeled []Assertion `json:"unlabeled"`
}

// GroupAll assembles one group per target version, in the order targets are
// given (the caller passes the version log's order), and files every row
// under the target it pins.
//
// Two totality rules, both deliberate:
//
//   - a target with no rows still gets its group. An empty answer is an
//     answer: "this version carries no assertions" is what the reader asked
//     for, and omitting the group would say nothing instead.
//   - a row whose pinned target was not listed gets its own group, appended
//     in first-seen order and carrying the pin with no version number. No
//     row is ever dropped (invariant 8: nothing disappears), and a pin the
//     caller could not name is still shown.
func GroupAll(targets []Target, rows []Assertion) []TargetGroup {
	index := make(map[string]int, len(targets))
	ordered := make([]Target, 0, len(targets))
	for _, t := range targets {
		if _, seen := index[t.ObjectVersionID]; seen {
			continue // a target named twice is one group
		}
		index[t.ObjectVersionID] = len(ordered)
		ordered = append(ordered, t)
	}
	buckets := make([][]Assertion, len(ordered))
	for _, row := range rows {
		i, ok := index[row.TargetObjectVersionID]
		if !ok {
			i = len(ordered)
			index[row.TargetObjectVersionID] = i
			ordered = append(ordered, Target{ObjectVersionID: row.TargetObjectVersionID})
			buckets = append(buckets, nil)
		}
		buckets[i] = append(buckets[i], row)
	}
	groups := make([]TargetGroup, 0, len(ordered))
	for i, t := range ordered {
		groups = append(groups, Group(t, buckets[i]))
	}
	return groups
}

// Group assembles one target's group: each row is labelled with the stance
// the domain rule gives its relation and filed under that stance's bucket.
//
// The labelling and the bucketing are the same call on purpose: a row can
// never be filed as supporting while carrying another label, or carry a
// label while sitting in Unlabeled. When the rule answers ok=false the row
// keeps no label at all (the field is omitted on the wire) and goes to
// Unlabeled — the fail-closed direction. A stance name this build cannot
// place (unreachable while domain.RelationStance returns the three canonical
// labels) is treated the same way rather than dropped: showing less is the
// safe error.
func Group(target Target, rows []Assertion) TargetGroup {
	g := TargetGroup{
		Target:     target,
		Supporting: []Assertion{},
		Contesting: []Assertion{},
		Neutral:    []Assertion{},
		Unlabeled:  []Assertion{},
	}
	for _, row := range rows {
		stance, ok := domain.RelationStance(domain.EvidenceRelation(row.Relation))
		if !ok {
			row.Stance = ""
			g.Unlabeled = append(g.Unlabeled, row)
			continue
		}
		row.Stance = string(stance)
		switch stance {
		case domain.EvidenceStanceSupporting:
			g.Supporting = append(g.Supporting, row)
		case domain.EvidenceStanceContesting:
			g.Contesting = append(g.Contesting, row)
		case domain.EvidenceStanceNeutral:
			g.Neutral = append(g.Neutral, row)
		default:
			// Unreachable today (the rule returns one of the three above
			// or ok=false). If a fourth stance ever appears, this read has
			// no bucket for it and refuses to guess: the row keeps no
			// label rather than carrying one the buckets contradict.
			row.Stance = ""
			g.Unlabeled = append(g.Unlabeled, row)
		}
	}
	return g
}

// ObjectRef names one scientific object (docs/08: every object is a
// versioned scientific object; this read names the object, not a pin).
type ObjectRef struct {
	ObjectID   string `json:"object_id"`
	ObjectType string `json:"object_type"`
	// Title is the title of the object's newest version, as the caller's
	// version read returned it.
	Title string `json:"title"`
}

// ObjectEvidence is the evidence of one scientific object: one group per
// pinned target version it carries assertions on.
type ObjectEvidence struct {
	ProjectID string        `json:"project_id"`
	Object    ObjectRef     `json:"object"`
	Groups    []TargetGroup `json:"groups"`
}

// ClaimRef names one claim in the hypothesis page's second section. Title is
// the title of the claim VERSION the subordinating relation pins (that is
// the version the edge was written about, and the one the relation read
// resolved).
type ClaimRef struct {
	ObjectID   string `json:"object_id"`
	ObjectType string `json:"object_type"`
	Title      string `json:"title"`
}

// ClaimEvidence is one subordinate claim and its own assertions, grouped by
// the claim version each assertion pins.
type ClaimEvidence struct {
	Claim    ClaimRef      `json:"claim"`
	Evidence []TargetGroup `json:"evidence"`
}

// HypothesisEvidence is the hypothesis page's read model: TWO SECTIONS, kept
// apart. Direct holds the assertions pinned to the hypothesis's own
// versions; Claims holds its subordinate claims, each carrying its own
// assertions.
//
// The two are never merged into one list and never combined into a count, a
// ratio or a net position. That is the Supervisor's ruling of 2026-09-18 and
// it follows from the requirement's own word (grouping): docs/10 §4 「V1 不自动
// 赋数值权重」 and CLAUDE.md §9.13 forbid the aggregate, and docs/08's
// Hypothesis entry gives the hypothesis historical relations to its claims
// (tested_by/supports), not a rule that folds them together. A hypothesis
// level view that DID merge them would be a new scientific-semantics
// decision and a task of its own, not something this projection may invent.
type HypothesisEvidence struct {
	ProjectID  string          `json:"project_id"`
	Hypothesis ObjectRef       `json:"hypothesis"`
	Direct     []TargetGroup   `json:"direct"`
	Claims     []ClaimEvidence `json:"claims"`
}
