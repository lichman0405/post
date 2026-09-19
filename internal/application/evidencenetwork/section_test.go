package evidencenetwork

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/lichman0405/post/internal/domain"
)

// The read model's own tests: which class an assertion lands in, that the
// class names ARE docs/10 §7's (pinned to the domain, not written twice
// here), that a row the network may not see is dropped rather than moved, and
// that nothing in the section is an aggregate.

const (
	ownProject   = "11111111-1111-4111-8111-111111111111"
	otherProject = "22222222-2222-4222-8222-222222222222"
	targetVer    = "33333333-3333-4333-8333-333333333333"
	evidenceVer  = "44444444-4444-4444-8444-444444444444"
)

func assertion(id, project, reviewState, visibility string) Assertion {
	return Assertion{
		ID:                      id,
		Relation:                "supports",
		Stance:                  "supporting",
		EvidenceType:            "experimental",
		Directness:              "direct",
		InferenceNature:         "deductive",
		Scope:                   json.RawMessage(`{}`),
		ReviewState:             reviewState,
		AssertingProjectID:      project,
		TargetObjectVersionID:   targetVer,
		EvidenceObjectVersionID: evidenceVer,
		CreatedAt:               "2026-09-19T00:00:00Z",
		SourceProjectVisibility: visibility,
	}
}

// TestSectionClassKeysAreTheDomainClasses: the three JSON keys the document
// presents are the three classes the domain names, in the domain's order. A
// list written twice is a list that can drift, and a document that grew a
// fourth bucket (or renamed one) would stop matching docs/10 §7 without any
// test noticing.
func TestSectionClassKeysAreTheDomainClasses(t *testing.T) {
	keys := CanonicalClassKeys()
	classes := domain.CanonicalEvidenceNetworkClasses()
	if len(classes) != 3 {
		t.Fatalf("the domain names %d classes, want the 3 of docs/10 §7", len(classes))
	}
	want := make([]string, 0, len(classes))
	for _, c := range classes {
		want = append(want, string(c))
	}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("the section's class keys are %v, want the domain's %v", keys, want)
	}

	// The rendered JSON must actually use them: a key renamed in the struct
	// tag would leave this test green if it only compared the constants. The
	// section rendered here is Build's EMPTY one — the shape a published
	// object with no evidence gets, which is what a reader must be able to
	// tell apart from "the field is not there".
	raw, err := json.Marshal(Build(ownProject, nil, false))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range want {
		value, present := decoded[key]
		if !present {
			t.Errorf("the rendered section has no %q key: %s", key, raw)
			continue
		}
		if rows, ok := value.([]any); !ok || len(rows) != 0 {
			t.Errorf("%q renders as %v, want an empty array (never null: an absent array reads as \"unknown\", an empty one as \"none\")", key, value)
		}
	}
}

// TestBuildBucketsEveryAssertionByTheTwoAxes: the class is a function of the
// two axes and nothing else — who asserted it (origin vs external) and how it
// was reviewed (review_state). Origin evidence stays origin whatever its
// review state; external evidence is split by review state.
func TestBuildBucketsEveryAssertionByTheTwoAxes(t *testing.T) {
	cases := []struct {
		name        string
		project     string
		visibility  string
		reviewState string
		want        string
	}{
		{"own project, unreviewed", ownProject, "public", "unreviewed", "origin"},
		{"own project, reviewed", ownProject, "public", "reviewed", "origin"},
		{"own project, rejected", ownProject, "public", "rejected", "origin"},
		{"other project, unreviewed", otherProject, "public", "unreviewed", "unreviewed_external"},
		{"other project, reviewed", otherProject, "public", "reviewed", "reviewed_external"},
		{"other project, rejected", otherProject, "public", "rejected", "unreviewed_external"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			section := Build(ownProject, []Assertion{assertion("a", c.project, c.reviewState, c.visibility)}, false)
			if got := len(bucket(section, c.want)); got != 1 {
				t.Fatalf("%s holds %d assertions, want the row (section: %+v)", c.want, got, section)
			}
		})
	}
}

// TestBuildRejectedExternalEvidenceIsNotReviewedExternal: the reviewed axis
// has THREE stored values and the class has two external buckets, so a
// rejected assertion must land in unreviewed_external. Folding it into
// "reviewed" would present the network with an assertion the platform's
// reviewers refused (docs/10 §7 distinguishes reviewed from unreviewed
// external evidence, and a rejected assertion is not a reviewed one).
func TestBuildRejectedExternalEvidenceIsNotReviewedExternal(t *testing.T) {
	section := Build(ownProject, []Assertion{
		assertion("rejected", otherProject, string(domain.EvidenceReviewRejected), "public"),
		assertion("reviewed", otherProject, string(domain.EvidenceReviewReviewed), "public"),
	}, false)
	if len(section.ReviewedExternal) != 1 || section.ReviewedExternal[0].ID != "reviewed" {
		t.Errorf("reviewed_external = %+v, want only the reviewed assertion", section.ReviewedExternal)
	}
	if len(section.UnreviewedExternal) != 1 || section.UnreviewedExternal[0].ID != "rejected" {
		t.Errorf("unreviewed_external = %+v, want the rejected assertion alongside the unreviewed ones", section.UnreviewedExternal)
	}
}

// TestBuildRefusesAnInvisibleProject'sEvidence is the fail-closed re-check:
// the store's predicate should already have excluded a private project's
// assertion, and this is the rule applied where the decision is made. The row
// is DROPPED, never reclassified — attributing another project's work to this
// document's origin would be the opposite error.
func TestBuildRefusesAnInvisibleProjectEvidence(t *testing.T) {
	section := Build(ownProject, []Assertion{
		assertion("visible", otherProject, "reviewed", "public"),
		assertion("invisible", otherProject, "reviewed", "private"),
	}, false)
	if len(section.ReviewedExternal) != 1 || section.ReviewedExternal[0].ID != "visible" {
		t.Errorf("reviewed_external = %+v, want only the visible project's assertion", section.ReviewedExternal)
	}
	if len(section.Origin) != 0 || len(section.UnreviewedExternal) != 0 {
		t.Errorf("the dropped row was reclassified: origin=%d unreviewed=%d", len(section.Origin), len(section.UnreviewedExternal))
	}
}

// TestBuildKeepsAnUnknownReviewStateVisible: the review-state vocabulary is
// constrained by the storage CHECK, so a value outside it is unreachable —
// and if one arrives anyway, the row is presented as UNREVIEWED external
// evidence rather than dropped. Dropping it would make a storage drift into a
// silent deletion, which is the one outcome docs/24 §2 forbids; presenting it
// in the bucket that claims the least about it is the fail-closed direction
// for the reader (they see the assertion and see that nobody has reviewed
// it).
func TestBuildKeepsAnUnknownReviewStateVisible(t *testing.T) {
	section := Build(ownProject, []Assertion{assertion("odd", otherProject, "something-new", "public")}, false)
	if len(section.UnreviewedExternal) != 1 {
		t.Errorf("unreviewed_external = %+v, want the row with the unrecognised review state", section.UnreviewedExternal)
	}
}

// TestBuildPreservesConflictAndOrder: two assertions on one (target,
// evidence) pair — one supporting, one contesting — both appear, each in its
// own bucket, in the order the read returned (newest first). Nothing is
// merged, adjudicated or counted.
func TestBuildPreservesConflictAndOrder(t *testing.T) {
	supporting := assertion("newer", otherProject, "unreviewed", "public")
	supporting.Relation, supporting.Stance = "supports", "supporting"
	contesting := assertion("older", otherProject, "unreviewed", "public")
	contesting.Relation, contesting.Stance = "contradicts", "contesting"

	section := Build(ownProject, []Assertion{supporting, contesting}, false)
	if len(section.UnreviewedExternal) != 2 {
		t.Fatalf("unreviewed_external holds %d assertions, want both sides of the conflict", len(section.UnreviewedExternal))
	}
	if section.UnreviewedExternal[0].ID != "newer" || section.UnreviewedExternal[1].ID != "older" {
		t.Errorf("the section reordered the read: %s, %s", section.UnreviewedExternal[0].ID, section.UnreviewedExternal[1].ID)
	}
	if section.UnreviewedExternal[0].Stance == section.UnreviewedExternal[1].Stance {
		t.Errorf("both stances came out as %q: the label must be the assertion's own", section.UnreviewedExternal[0].Stance)
	}
}

// TestSectionCarriesNoAggregate: the section is a list of facts. No field of
// an assertion or of the section is a number, and no rendered key is a score
// — CLAUDE.md §9.13 forbids a Truth Score outright and docs/10 §8's four
// summary labels are an owner decision this build does not make.
func TestSectionCarriesNoAggregate(t *testing.T) {
	section := Build(ownProject, []Assertion{
		assertion("own", ownProject, "reviewed", "public"),
		assertion("theirs", otherProject, "reviewed", "public"),
	}, true)
	raw, err := json.Marshal(section)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["truncated"] != true {
		t.Errorf("truncated = %v, want the flag the read reported", decoded["truncated"])
	}
	delete(decoded, "truncated")
	for key, value := range decoded {
		rows, ok := value.([]any)
		if !ok {
			t.Errorf("%q is %T, want an array", key, value)
			continue
		}
		for _, row := range rows {
			fields, _ := row.(map[string]any)
			for name, field := range fields {
				if _, numeric := field.(float64); numeric {
					t.Errorf("%s.%s is numeric (%v): the section carries facts, never an aggregate", key, name, field)
				}
			}
		}
	}
}

// TestStanceOfLabelsOnlyCanonicalRelations: the stance is the domain's own
// derivation from the relation, and a relation the catalog does not hold is
// rendered with no label rather than with an invented one.
func TestStanceOfLabelsOnlyCanonicalRelations(t *testing.T) {
	if got := StanceOf(string(domain.EvidenceRelationSupports)); got != string(domain.EvidenceStanceSupporting) {
		t.Errorf("supports → %q, want %q", got, domain.EvidenceStanceSupporting)
	}
	if got := StanceOf(string(domain.EvidenceRelationContradicts)); got != string(domain.EvidenceStanceContesting) {
		t.Errorf("contradicts → %q, want %q", got, domain.EvidenceStanceContesting)
	}
	if got := StanceOf("not-a-relation"); got != "" {
		t.Errorf("an unknown relation → %q, want no label", got)
	}
}

// bucket returns the assertions of one class bucket by its key.
func bucket(s Section, key string) []Assertion {
	switch key {
	case "origin":
		return s.Origin
	case "reviewed_external":
		return s.ReviewedExternal
	case "unreviewed_external":
		return s.UnreviewedExternal
	default:
		return nil
	}
}
