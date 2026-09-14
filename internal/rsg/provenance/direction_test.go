package provenance

import (
	"sort"
	"testing"

	"github.com/lichman0405/post/internal/rsg/relationcatalog"
)

// TestDirectionMatchesCatalog pins the two copies of the provenance type
// set together, both ways: every type the catalog marks for provenance
// inference must have a declared origin side here, and nothing else may.
// A future catalog change that forgets to declare the direction — or
// declares one for a knowledge/weak type — fails this test instead of
// silently mis-walking a graph.
func TestDirectionMatchesCatalog(t *testing.T) {
	var catalog []string
	for _, e := range relationcatalog.Types() {
		if e.ProvenanceInference {
			catalog = append(catalog, e.Type)
		}
	}
	sort.Strings(catalog)

	var declared []string
	for typ := range originSide {
		declared = append(declared, typ)
	}
	sort.Strings(declared)

	if len(catalog) != len(declared) {
		t.Fatalf("direction set size %d != catalog provenance set size %d\ncatalog:   %v\ndeclared:  %v",
			len(declared), len(catalog), catalog, declared)
	}
	for i := range catalog {
		if catalog[i] != declared[i] {
			t.Fatalf("direction set diverges from catalog provenance set\ncatalog:   %v\ndeclared:  %v", catalog, declared)
		}
	}
}

// TestOriginAtSourceTypes pins the three reversed types: the provenance
// edges whose SOURCE endpoint is the origin side — produces (the producer),
// used_by (the thing being used) and part_of (the part an aggregate is
// assembled from, the dual of contains). Everything else points origin at
// the target — TestOriginOfPointsAtTarget checks that half.
func TestOriginAtSourceTypes(t *testing.T) {
	for _, typ := range []string{"produces", "used_by", "part_of"} {
		if got := originSide[typ]; got != OriginIsSource {
			t.Errorf("originSide[%s] = %v, want OriginIsSource", typ, got)
		}
	}
}

// TestPartOfContainsShareOrigin pins the duality the catalog declares
// (relationcatalog: contains — "contains the target object version as a
// part"; part_of — "is part of the target aggregate or composite"): the
// same scientific fact recorded either way must resolve the SAME origin —
// the part. "A contains B" and "B part_of A" both answer origin = B.
func TestPartOfContainsShareOrigin(t *testing.T) {
	aggregate := node("agg", "agg-v1", 1, "dataset", "Aggregate")
	part := node("part", "part-v1", 1, "material", "Part")

	origin, derived, ok := originOf(Edge{RelationType: "contains", Source: aggregate, Target: part})
	if !ok || origin != part || derived != aggregate {
		t.Errorf("contains: originOf = (%v, %v, %v), want (part, aggregate, true)", origin, derived, ok)
	}
	origin, derived, ok = originOf(Edge{RelationType: "part_of", Source: part, Target: aggregate})
	if !ok || origin != part || derived != aggregate {
		t.Errorf("part_of: originOf = (%v, %v, %v), want (part, aggregate, true)", origin, derived, ok)
	}
}

// TestOriginOfPointsAtTarget checks the non-reversed types: X <typ> Y means
// X came from Y, so originOf returns (target, source).
func TestOriginOfPointsAtTarget(t *testing.T) {
	for _, e := range relationcatalog.Types() {
		if !e.ProvenanceInference {
			continue
		}
		if e.Type == "produces" || e.Type == "used_by" || e.Type == "part_of" {
			continue
		}
		src := node("src-object", "src-v1", 1, "dataset", "source")
		tgt := node("tgt-object", "tgt-v1", 1, "protocol", "target")
		origin, derived, ok := originOf(Edge{RelationType: e.Type, Source: src, Target: tgt})
		if !ok {
			t.Errorf("%s: no declared origin side", e.Type)
			continue
		}
		if origin != tgt || derived != src {
			t.Errorf("%s: originOf = (%v, %v), want (target, source)", e.Type, origin, derived)
		}
	}
}

// TestUndeclaredTypeHasNoOriginSide: knowledge and weak types never enter
// the walk — originOf reports them undeclared.
func TestUndeclaredTypeHasNoOriginSide(t *testing.T) {
	for _, typ := range []string{"supports", "contradicts", "related_to", "materials:custom"} {
		if _, _, ok := originOf(Edge{RelationType: typ}); ok {
			t.Errorf("%s: originOf declared, want undeclared", typ)
		}
	}
}
