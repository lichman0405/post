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

// TestDependencyInferenceDirections pins the two ends of every
// dependency-inference edge, which is what the impact analysis walks.
//
// The flag alone is not enough to walk with: it says "a change upstream
// must trigger re-analysis downstream" and does not say which end is
// downstream. depends_on names its DEPENDENCY as the target ("uses the
// target as an actual input") and used_by names it as the source ("is used
// by the target"), so a walker that assumed one direction would report the
// objects UPSTREAM of a change as the ones affected by it.
//
// The test asserts both halves of the contract on every entry:
//
//   - a type carrying DependencyInference declares a direction, and its two
//     ends are different;
//   - a type NOT carrying the flag declares neither end — references is the
//     one that matters, because it is the citation whose upstream changes
//     only notify (docs/19 §3), and it sits one field away from depends_on
//     in the table above.
func TestDependencyInferenceDirections(t *testing.T) {
	want := map[string][2]Endpoint{
		"depends_on": {EndpointTarget, EndpointSource},
		"used_by":    {EndpointSource, EndpointTarget},
	}
	for typ, ends := range want {
		e, ok := Lookup(typ)
		if !ok {
			t.Fatalf("catalog lost type %q", typ)
		}
		if !e.DependencyInference {
			t.Fatalf("%s must carry DependencyInference (docs/19 §3)", typ)
		}
		if e.DependencyEnd != ends[0] || e.DependentEnd != ends[1] {
			t.Errorf("%s direction = dependency:%q dependent:%q, want dependency:%q dependent:%q",
				typ, e.DependencyEnd, e.DependentEnd, ends[0], ends[1])
		}
		dependency, dependent, ok := DependencyDirection(typ)
		if !ok {
			t.Fatalf("DependencyDirection(%s) rejected a flagged type", typ)
		}
		if dependency != ends[0] || dependent != ends[1] {
			t.Errorf("DependencyDirection(%s) = %q,%q, want %q,%q", typ, dependency, dependent, ends[0], ends[1])
		}
	}
	for _, e := range Types() {
		if e.DependencyInference {
			if e.DependencyEnd == "" || e.DependentEnd == "" {
				t.Errorf("%s carries DependencyInference with an undeclared direction (%q -> %q): the walk cannot tell which end is downstream",
					e.Type, e.DependencyEnd, e.DependentEnd)
			}
			if e.DependencyEnd == e.DependentEnd {
				t.Errorf("%s declares the same endpoint (%q) as both dependency and dependent", e.Type, e.DependencyEnd)
			}
			continue
		}
		if e.DependencyEnd != "" || e.DependentEnd != "" {
			t.Errorf("%s declares a dependency direction without the flag; the flag is what says \"a change upstream triggers re-analysis\" (docs/19 §3)",
				e.Type)
		}
		if _, _, ok := DependencyDirection(e.Type); ok {
			t.Errorf("DependencyDirection(%s) accepted a type without the flag", e.Type)
		}
	}
	if _, _, ok := DependencyDirection("no_such_type"); ok {
		t.Error("DependencyDirection accepted an unknown type; it must fail closed")
	}
}

// TestDependentTypesSplitsByDeclaredEnd pins DependentTypes to the
// declarations above: the two sets are disjoint (an end is declared once
// each way, never both), and a flagged type with no direction appears in
// NEITHER set — its edges are not walked at all, which is why the test
// above fails on such a declaration rather than leaving the hole silent.
func TestDependentTypesSplitsByDeclaredEnd(t *testing.T) {
	sources := DependentTypes(EndpointSource)
	targets := DependentTypes(EndpointTarget)
	if !reflect.DeepEqual(sources, []string{"depends_on"}) {
		t.Errorf("DependentTypes(source) = %v, want [depends_on] (the type whose dependent is the edge's source)", sources)
	}
	if !reflect.DeepEqual(targets, []string{"used_by"}) {
		t.Errorf("DependentTypes(target) = %v, want [used_by] (the type whose dependent is the edge's target)", targets)
	}
	seen := map[string]bool{}
	for _, typ := range sources {
		seen[typ] = true
	}
	for _, typ := range targets {
		if seen[typ] {
			t.Errorf("type %q appears in both dependent sets; one end is declared once", typ)
		}
	}
	// references is the citation whose changes only notify: it must be in
	// neither set, and that is the fact the impact analysis's acceptance
	// turns into a measured failure by flipping the flag.
	for _, set := range [][]string{sources, targets} {
		for _, typ := range set {
			if typ == "references" {
				t.Errorf("references appears in a dependent set (%v): its upstream changes only notify (docs/19 §3)", set)
			}
		}
	}
	if DependentTypes("middle") != nil {
		t.Error("DependentTypes returned a set for an endpoint that does not exist")
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
