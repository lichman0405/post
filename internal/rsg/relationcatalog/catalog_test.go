package relationcatalog

import (
	"reflect"
	"strings"
	"testing"
)

// The two knowledge edges declare only the endpoint their NAME pins — the
// target ("addresses the target research question", "tests the target
// hypothesis"); the source end stays unconstrained, because no spec says
// who may point at a question or a hypothesis. Everything else declares
// neither end — a constraint on e.g. derived_from would be a product
// decision nobody has made, so the catalog must not invent one.
func TestKnowledgeEdgesDeclareEndpointTypes(t *testing.T) {
	cases := []struct {
		typ     string
		sources []string
		targets []string
	}{
		{"addresses_question", nil, []string{"research_question"}},
		{"tests_hypothesis", nil, []string{"hypothesis"}},
	}
	for _, c := range cases {
		e, ok := Lookup(c.typ)
		if !ok {
			t.Fatalf("Lookup(%s) not found", c.typ)
		}
		if e.Category != CategoryKnowledge {
			t.Errorf("%s category = %s, want knowledge", c.typ, e.Category)
		}
		if len(e.SourceTypes) != len(c.sources) || len(e.TargetTypes) != len(c.targets) {
			t.Fatalf("%s endpoints = %v -> %v, want %v -> %v", c.typ, e.SourceTypes, e.TargetTypes, c.sources, c.targets)
		}
		for i := range c.sources {
			if e.SourceTypes[i] != c.sources[i] {
				t.Fatalf("%s source endpoint [%d] = %s, want %s", c.typ, i, e.SourceTypes[i], c.sources[i])
			}
		}
		for i := range c.targets {
			if e.TargetTypes[i] != c.targets[i] {
				t.Fatalf("%s target endpoint [%d] = %s, want %s", c.typ, i, e.TargetTypes[i], c.targets[i])
			}
		}
	}
	for _, e := range Types() {
		if e.Type == "addresses_question" || e.Type == "tests_hypothesis" {
			continue
		}
		if len(e.SourceTypes) != 0 || len(e.TargetTypes) != 0 {
			t.Errorf("%s declares endpoint types %v -> %v; only the two knowledge edges may declare endpoints, and only their name-pinned targets", e.Type, e.SourceTypes, e.TargetTypes)
		}
	}
}

func TestEndpointsValid(t *testing.T) {
	cases := []struct {
		typ, src, tgt string
		ok            bool
		wantMsg       string
	}{
		{"addresses_question", "hypothesis", "research_question", true, ""},
		{"addresses_question", "finding", "research_question", true, ""}, // the main-line shape: sources are not restricted
		{"tests_hypothesis", "experiment", "hypothesis", true, ""},
		{"tests_hypothesis", "calculation", "hypothesis", true, ""},
		{"tests_hypothesis", "hypothesis", "hypothesis", true, ""}, // sources are not restricted
		{"addresses_question", "material", "claim", false, "target object type claim is not allowed"},
		{"tests_hypothesis", "calculation", "research_question", false, "target object type research_question is not allowed"},
		{"unknown:type", "a", "b", false, "not a canonical relation type"},
		{"derived_from", "anything", "goes", true, ""}, // unconstrained
	}
	for _, c := range cases {
		ok, msg := EndpointsValid(c.typ, c.src, c.tgt)
		if ok != c.ok {
			t.Errorf("EndpointsValid(%s, %s, %s) = %v, want %v (%s)", c.typ, c.src, c.tgt, ok, c.ok, msg)
		}
		if !c.ok && !strings.Contains(msg, c.wantMsg) {
			t.Errorf("EndpointsValid(%s, %s, %s) message = %q, want it to contain %q", c.typ, c.src, c.tgt, msg, c.wantMsg)
		}
	}
}

// TestExternalRefTargetTypes pins the citation/dependency pair of
// docs/19 §3: the ONLY relation types that may point AT an external
// reference are references (background knowledge; upstream changes only
// notify) and depends_on (actual input; upstream changes trigger impact
// analysis). The database declares the same set
// (external_reference_relation_types, migration 00045); the integration
// drift test keeps the two copies in lockstep — this test makes a
// catalog change fail loudly HERE first.
func TestExternalRefTargetTypes(t *testing.T) {
	got := ExternalRefTargetTypes()
	want := []string{"depends_on", "references"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExternalRefTargetTypes() = %v, want %v (docs/19 §3: only the citation/dependency pair may target an external reference)", got, want)
	}
	// The pair must be complete in the catalog: each is a core type, each
	// marked, and no other core type may be marked (the sorted equality
	// above covers "no other", the entries below cover "each marked").
	for _, typ := range want {
		e, ok := Lookup(typ)
		if !ok {
			t.Fatalf("catalog lost core type %q", typ)
		}
		if !e.ExternalRefTarget {
			t.Errorf("type %q must be marked ExternalRefTarget", typ)
		}
	}
}
