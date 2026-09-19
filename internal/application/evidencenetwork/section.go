package evidencenetwork

import (
	"encoding/json"

	"github.com/lichman0405/post/internal/domain"
)

// The section of the published knowledge document that presents the
// object's evidence (docs/10 §7, docs/31 Gate D). See the package comment
// for the split between this read model, the evidence table, and the write
// command.

// MaxAssertions bounds one evidence section. The evidence on a popular
// published object is unbounded, and an unbounded read is a resource a
// writer can exhaust; the store reads one row beyond this and the section
// reports whether it was cut (Truncated), so a short list is never
// presented as a complete one.
const MaxAssertions = 200

// Assertion is one evidence assertion as the network read renders it: the
// row's own facts, and the two inputs the classification needs (the
// asserting project's id, compared against the published version's owning
// project, and the review state).
//
// It deliberately carries NO class field: the class is exactly the bucket
// the assertion is presented in, and a copy of it on the row could disagree
// with the bucket it sits in.
//
// The JSON shape is this type's own (the same decision knowledgepublish.
// Preview makes): the transport renders it as-is, so a field cannot be
// dropped in a layer that has no test for it.
type Assertion struct {
	ID string `json:"id"`
	// Relation is one of docs/10 §4's nine evidence relations, and Stance
	// is the transparent per-assertion label the domain derives from it
	// (relation → supporting/contesting/neutral). It is a LABEL, never a
	// weight: nothing in this document is summed, averaged or ranked, and
	// two assertions on one pair keep opposite stances side by side.
	Relation string `json:"relation"`
	Stance   string `json:"stance,omitempty"`
	// EvidenceType, Directness and InferenceNature are the schema's
	// declarations; Scope bounds where the assertion holds.
	EvidenceType    string          `json:"evidence_type"`
	Directness      string          `json:"directness"`
	InferenceNature string          `json:"inference_nature"`
	Scope           json.RawMessage `json:"scope"`
	ReasoningNote   string          `json:"reasoning_note"`
	// ReviewState is the reviewer's position on THIS assertion
	// (unreviewed/reviewed/rejected). It is shown as stored — a rejected
	// assertion is not hidden (invariant 8: nothing disappears; state only
	// evolves) and it is not laundered into "reviewed".
	ReviewState string `json:"review_state"`
	// AssertingProjectID is the project that MADE the assertion (the
	// source of the origin/external axis); the project that owns the
	// published version is the document's own project.
	AssertingProjectID string `json:"asserting_project_id"`
	// TargetObjectVersionID is the published version the assertion is
	// about, and EvidenceObjectVersionID the version it cites. Both are
	// version pins: the assertion keeps its meaning when either object
	// moves on (docs/10 §3).
	TargetObjectVersionID   string `json:"target_object_version_id"`
	EvidenceObjectVersionID string `json:"evidence_object_version_id"`
	// CreatedAt is the assertion's creation instant, formatted by the
	// caller (knowledgepublish.FormatInstant) — the same wire form the rest
	// of the document uses.
	CreatedAt string `json:"created_at"`

	// SourceProjectVisibility is the asserting project's preset as the
	// store read it. It is NOT rendered (`json:"-"`): it is an input of the
	// fail-closed re-check in Build, and the SQL predicate that already
	// filtered on it is the strategy, not the rule.
	SourceProjectVisibility string `json:"-"`
}

// Section is the evidence part of a published knowledge document: the three
// classes, presented SEPARATELY. Each is an array, always present and never
// null, in the order docs/10 §7 names them — a rendered document that
// merged them into one list would lose the distinction the class makes, and
// a missing array would read as "no evidence of that kind" only by
// accident.
type Section struct {
	Origin             []Assertion `json:"origin"`
	ReviewedExternal   []Assertion `json:"reviewed_external"`
	UnreviewedExternal []Assertion `json:"unreviewed_external"`
	// Truncated reports that the section was cut at MaxAssertions (the
	// oldest assertions are the ones dropped). It is stated rather than
	// left to be inferred from a suspiciously round list length.
	Truncated bool `json:"truncated"`
}

// Build assembles the section: it re-checks each row against the rule and
// places it in its class.
//
// targetProjectID is the project that OWNS the published object version the
// rows were read for — the origin side of axis 1.
//
// The re-check is the point of this function. The store's predicate
// (ListPublishedEvidenceForTarget) already excludes rows the network may not
// see; that predicate is a read strategy, and this is the rule, so a row
// that slipped past it — written by a path that never went through the
// evidence command, or read by a query that drifted — is still refused
// HERE, where the decision is made. A dropped row is the fail-closed
// direction: the reader sees less, never more.
func Build(targetProjectID string, rows []Assertion, truncated bool) Section {
	section := Section{
		Origin:             []Assertion{},
		ReviewedExternal:   []Assertion{},
		UnreviewedExternal: []Assertion{},
		Truncated:          truncated,
	}
	for _, row := range rows {
		external := domain.AssertionIsExternal(row.AssertingProjectID, targetProjectID)
		if external && row.SourceProjectVisibility != string(domain.VisibilityPublic) {
			// Another project's assertion is rendered only when that
			// project is public: a private project's work is not published
			// by asserting it somewhere else (docs/12 §2 发布不等于公开).
			continue
		}
		switch domain.ClassifyEvidenceNetwork(external, domain.EvidenceReviewState(row.ReviewState)) {
		case domain.EvidenceClassReviewedExternal:
			section.ReviewedExternal = append(section.ReviewedExternal, row)
		case domain.EvidenceClassUnreviewedExternal:
			section.UnreviewedExternal = append(section.UnreviewedExternal, row)
		default:
			// EvidenceClassOrigin, and the fail-closed default for anything
			// else: an assertion in the published object's own project is
			// the origin's own evidence, whatever its review state
			// (ClassifyEvidenceNetwork is total, so this branch is the
			// origin case and an unknown class name can never drop a row).
			section.Origin = append(section.Origin, row)
		}
	}
	return section
}

// CanonicalClassKeys returns the JSON keys of the three class arrays, in the
// order the document presents them. It exists so a test can pin the section
// to domain.CanonicalEvidenceNetworkClasses — the classes docs/10 §7 names —
// instead of a list written twice.
func CanonicalClassKeys() []string {
	return []string{
		string(domain.EvidenceClassOrigin),
		string(domain.EvidenceClassReviewedExternal),
		string(domain.EvidenceClassUnreviewedExternal),
	}
}

// StanceOf maps one stored relation to its transparent stance label ("" when
// the relation is not a canonical one — the storage CHECK makes that
// unreachable, and an unknown relation is rendered without a label rather
// than with an invented one).
func StanceOf(relation string) string {
	stance, ok := domain.RelationStance(domain.EvidenceRelation(relation))
	if !ok {
		return ""
	}
	return string(stance)
}
