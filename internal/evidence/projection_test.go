package evidence

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/domain"
)

// Task T0506 — the evidence-graph read model's own rules, pinned without a
// database. Every test here is written to be able to fail: the assertions
// name the bucket a row must (and must NOT) appear in, and the two scans
// below carry a self-check over a document that violates them, so a scan
// that forbade nothing cannot read green.

func target(versionID string, no int) Target {
	return Target{ObjectVersionID: versionID, VersionNo: &no, Title: "v" + strings.Repeat("x", no)}
}

func row(id, targetVersionID, relation string) Assertion {
	return Assertion{
		ID:                    id,
		Relation:              relation,
		EvidenceType:          "experimental",
		Scope:                 json.RawMessage(`{}`),
		ReviewState:           "unreviewed",
		TargetObjectVersionID: targetVersionID,
		CreatedAt:             "2026-09-19T00:00:00.000Z",
	}
}

func ids(rows []Assertion) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

func groupFor(t *testing.T, g TargetGroup, versionID string) TargetGroup {
	t.Helper()
	if g.Target.ObjectVersionID != versionID {
		t.Fatalf("group target = %s, want %s", g.Target.ObjectVersionID, versionID)
	}
	return g
}

// TestGroupsAreKeyedByPinnedTargetVersion is the "grouping, not union" rule:
// rows pinning different versions end up in different groups, in the order
// the caller's version log gives, and a row is only ever in the group of the
// version it pins.
func TestGroupsAreKeyedByPinnedTargetVersion(t *testing.T) {
	rows := []Assertion{
		row("a1", "v1", "supports"),
		row("b1", "v2", "contradicts"),
		row("a2", "v1", "contextualizes"),
	}
	groups := GroupAll([]Target{target("v1", 1), target("v2", 2)}, rows)
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want one per pinned target (2)", len(groups))
	}
	first := groupFor(t, groups[0], "v1")
	if got := ids(first.Supporting); len(got) != 1 || got[0] != "a1" {
		t.Errorf("v1 supporting = %v, want [a1]", got)
	}
	if got := ids(first.Neutral); len(got) != 1 || got[0] != "a2" {
		t.Errorf("v1 neutral = %v, want [a2]", got)
	}
	second := groupFor(t, groups[1], "v2")
	if got := ids(second.Contesting); len(got) != 1 || got[0] != "b1" {
		t.Errorf("v2 contesting = %v, want [b1]", got)
	}
	if len(second.Supporting) != 0 {
		t.Errorf("v2 supporting = %v: a row may not appear under a version it does not pin", ids(second.Supporting))
	}
}

// TestSupportAndContestingOnOnePairAreNeverMerged is the design 00058's
// header protects: one evidence version may carry a supports AND a
// contradicts assertion on the SAME target version, and both must survive,
// each in its own bucket, neither collapsed nor adjudicated.
func TestSupportAndContestingOnOnePairAreNeverMerged(t *testing.T) {
	pair := []Assertion{
		{ID: "sup", Relation: "supports", TargetObjectVersionID: "tv", EvidenceObjectVersionID: "ev",
			EvidenceType: "experimental", Scope: json.RawMessage(`{}`), CreatedAt: "t"},
		{ID: "con", Relation: "contradicts", TargetObjectVersionID: "tv", EvidenceObjectVersionID: "ev",
			EvidenceType: "experimental", Scope: json.RawMessage(`{}`), CreatedAt: "t"},
	}
	g := Group(target("tv", 1), pair)
	if got := ids(g.Supporting); len(got) != 1 || got[0] != "sup" {
		t.Errorf("supporting = %v, want [sup]", got)
	}
	if got := ids(g.Contesting); len(got) != 1 || got[0] != "con" {
		t.Errorf("contesting = %v, want [con]", got)
	}
	// The failure this test exists to catch: a "neutral" merge of the two,
	// or either row bleeding into the other's bucket.
	for _, bucket := range [][]Assertion{g.Neutral, g.Unlabeled} {
		if len(bucket) != 0 {
			t.Errorf("the pair landed in %v: the two stances must stay in their own buckets", ids(bucket))
		}
	}
	if len(g.Supporting) == 1 && g.Supporting[0].Stance != string(domain.EvidenceStanceSupporting) {
		t.Errorf("supporting label = %q", g.Supporting[0].Stance)
	}
	if len(g.Contesting) == 1 && g.Contesting[0].Stance != string(domain.EvidenceStanceContesting) {
		t.Errorf("contesting label = %q", g.Contesting[0].Stance)
	}
}

// TestEveryCanonicalRelationLandsInItsDomainBucket pins the mapping to the
// domain rule rather than to a second copy of it written here: for each of
// the nine canonical relations, the bucket is the one domain.RelationStance
// names, and none of the nine may reach Unlabeled.
func TestEveryCanonicalRelationLandsInItsDomainBucket(t *testing.T) {
	for _, rel := range domain.CanonicalEvidenceRelations() {
		want, ok := domain.RelationStance(domain.EvidenceRelation(rel))
		if !ok {
			t.Fatalf("domain.RelationStance(%q) has no answer: the canonical nine must all map", rel)
		}
		g := Group(target("tv", 1), []Assertion{row("r", "tv", rel)})
		var got string
		switch {
		case len(g.Supporting) == 1:
			got = "supporting"
		case len(g.Contesting) == 1:
			got = "contesting"
		case len(g.Neutral) == 1:
			got = "neutral"
		default:
			t.Fatalf("relation %q landed in unlabeled (%v)", rel, ids(g.Unlabeled))
		}
		if got != string(want) {
			t.Errorf("relation %q: bucket %s, domain rule says %s", rel, got, want)
		}
	}
}

// TestUnmappedRelationKeepsNoStanceLabel is the fail-closed rule: a relation
// the domain rule does not map (RelationStance answers ok=false) is rendered
// without a stance label and filed away from all three stance buckets —
// never guessed into neutral.
func TestUnmappedRelationKeepsNoStanceLabel(t *testing.T) {
	// "supersedes" is a canonical RSG relation and is NOT one of the nine
	// evidence relations, which is exactly the shape a widened enum would
	// produce; "" is the shape a missing value would.
	for _, relation := range []string{"supersedes", ""} {
		g := Group(target("tv", 1), []Assertion{row("r", "tv", relation)})
		if len(g.Unlabeled) != 1 {
			t.Fatalf("relation %q: unlabeled = %v, want the row (it must still be rendered)", relation, ids(g.Unlabeled))
		}
		if g.Unlabeled[0].Stance != "" {
			t.Errorf("relation %q carries stance %q: an unmapped relation must carry none",
				relation, g.Unlabeled[0].Stance)
		}
		for name, bucket := range map[string][]Assertion{
			"supporting": g.Supporting, "contesting": g.Contesting, "neutral": g.Neutral,
		} {
			if len(bucket) != 0 {
				t.Errorf("relation %q was forced into %s", relation, name)
			}
		}
		// The wire is where the rule has to hold: the stance key must be
		// ABSENT, not an empty string, so a client cannot read "" as a
		// label it failed to parse.
		raw, err := json.Marshal(g)
		if err != nil {
			t.Fatalf("marshal group: %v", err)
		}
		if strings.Contains(string(raw), `"stance"`) {
			t.Errorf("relation %q renders a stance key: %s", relation, raw)
		}
	}
}

// TestContextualizesIsTheOnlyNeutral — neutral belongs to contextualizes
// alone (the domain rule's own statement), and the other eight never reach
// it.
func TestContextualizesIsTheOnlyNeutral(t *testing.T) {
	for _, rel := range domain.CanonicalEvidenceRelations() {
		g := Group(target("tv", 1), []Assertion{row("r", "tv", rel)})
		if wantNeutral := rel == "contextualizes"; wantNeutral != (len(g.Neutral) == 1) {
			t.Errorf("relation %q: neutral bucket has %d rows, want neutral=%v", rel, len(g.Neutral), wantNeutral)
		}
	}
}

// TestEmptyTargetGetsItsGroupAndUnknownPinsAreNotDropped pins the two
// totality rules of GroupAll: a named target that carries nothing still
// answers, and a row whose target was not named still appears (invariant 8:
// nothing disappears).
func TestEmptyTargetGetsItsGroupAndUnknownPinsAreNotDropped(t *testing.T) {
	groups := GroupAll([]Target{target("v1", 1), target("v2", 2)}, []Assertion{
		row("stray", "v9", "supports"),
	})
	if len(groups) != 3 {
		t.Fatalf("groups = %d, want 3 (two named targets and the stray pin)", len(groups))
	}
	empty := groupFor(t, groups[1], "v2")
	if len(empty.Supporting)+len(empty.Contesting)+len(empty.Neutral)+len(empty.Unlabeled) != 0 {
		t.Errorf("v2 is not empty: %+v", empty)
	}
	stray := groupFor(t, groups[2], "v9")
	if got := ids(stray.Supporting); len(got) != 1 || got[0] != "stray" {
		t.Errorf("the unlisted pin lost its row: %v", got)
	}
	if stray.Target.VersionNo != nil {
		t.Errorf("the unlisted pin invented a version number: %v", *stray.Target.VersionNo)
	}
}

// --------------------------------------------------------------------------
// The no-arithmetic scan

// forbiddenKeys are the shapes docs/10 §4 and CLAUDE.md §9.13 forbid. They
// are checked as substrings, case-insensitively, so a field named
// supporting_count, total_evidence, net_stance or truth_score cannot slip
// through whatever it is spelled.
var forbiddenKeys = []string{
	"count", "total", "score", "ratio", "weight", "net", "sum", "average",
	"percent", "confidence", "rank", "summary", "stancestrength",
}

// scanKeys walks raw JSON and reports every key it sees. The scope object is
// NOT skipped here (the caller decides what to do with it).
func scanKeys(t *testing.T, raw []byte, visit func(key string)) {
	t.Helper()
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("scan: unmarshal: %v", err)
	}
	var walk func(node any)
	walk = func(node any) {
		switch v := node.(type) {
		case map[string]any:
			for k, child := range v {
				visit(k)
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(doc)
}

// TestNoAggregateKeyOnTheWire marshals both read models and refuses any key
// that names a count, ratio, weight or score. The scan is proved able to
// fail first, on a document that violates it.
func TestNoAggregateKeyOnTheWire(t *testing.T) {
	// Self-check: the scan must flag a document that carries the forbidden
	// shape. Without this, "no forbidden key found" would be indistinguishable
	// from "the scan never looked".
	var flagged []string
	scanKeys(t, []byte(`{"groups":[{"supporting_count":1,"net":0}]}`), func(k string) {
		for _, bad := range forbiddenKeys {
			if strings.Contains(strings.ToLower(k), bad) {
				flagged = append(flagged, k)
			}
		}
	})
	if len(flagged) != 2 {
		t.Fatalf("the scan is not measuring: it flagged %v, want both planted keys", flagged)
	}

	payloads := map[string]any{
		"object read": ObjectEvidence{
			ProjectID: "p", Object: ObjectRef{ObjectID: "o", ObjectType: "claim", Title: "t"},
			Groups: GroupAll([]Target{target("v1", 1)}, []Assertion{
				row("a", "v1", "supports"), row("b", "v1", "contradicts"),
			}),
		},
		"hypothesis page": HypothesisEvidence{
			ProjectID: "p", Hypothesis: ObjectRef{ObjectID: "h", ObjectType: "hypothesis", Title: "t"},
			Direct: GroupAll([]Target{target("v1", 1)}, []Assertion{row("a", "v1", "supports")}),
			Claims: []ClaimEvidence{{Claim: ClaimRef{ObjectID: "c", ObjectType: "claim", Title: "c"},
				Evidence: GroupAll([]Target{target("v2", 2)}, []Assertion{row("d", "v2", "contradicts")})}},
		},
	}
	for name, payload := range payloads {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		scanKeys(t, raw, func(k string) {
			for _, bad := range forbiddenKeys {
				if strings.Contains(strings.ToLower(k), bad) {
					t.Errorf("%s: forbidden key %q on the wire: %s", name, k, raw)
				}
			}
		})
	}
}

// TestTheOnlyNumberOnTheWireIsTheVersionPin is the sharpest form of the same
// rule: the read model's own fields carry no number at all except the
// version number of a pin. A count of any kind would be a number, and there
// is none. The author's own scope object is skipped — it is JSON the author
// wrote, passed through untouched, not a number this platform derived.
func TestTheOnlyNumberOnTheWireIsTheVersionPin(t *testing.T) {
	scanNumbers := func(raw []byte, visit func(key string, n float64)) {
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("scan numbers: %v", err)
		}
		var walk func(node any, key string)
		walk = func(node any, key string) {
			switch v := node.(type) {
			case map[string]any:
				for k, child := range v {
					if k == "scope" {
						continue // the author's own JSON, passed through
					}
					walk(child, k)
				}
			case []any:
				for _, child := range v {
					walk(child, key)
				}
			case float64:
				visit(key, v)
			}
		}
		walk(doc, "")
	}

	// Self-check on a planted count.
	planted := 0
	scanNumbers([]byte(`{"groups":[{"supporting_count":2}]}`), func(k string, _ float64) {
		if k != "version_no" {
			planted++
		}
	})
	if planted != 1 {
		t.Fatalf("the number scan is not measuring: it saw %d non-pin numbers, want 1", planted)
	}

	raw, err := json.Marshal(HypothesisEvidence{
		ProjectID: "p", Hypothesis: ObjectRef{ObjectID: "h", ObjectType: "hypothesis"},
		Direct: GroupAll([]Target{target("v1", 1)}, []Assertion{row("a", "v1", "supports")}),
		Claims: []ClaimEvidence{{Claim: ClaimRef{ObjectID: "c"}, Evidence: GroupAll([]Target{target("v2", 2)},
			[]Assertion{row("d", "v2", "contradicts")})}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	scanNumbers(raw, func(k string, n float64) {
		if k != "version_no" {
			t.Errorf("the read model carries a number under %q (%v): %s", k, n, raw)
		}
	})
}
