package provenance

import (
	"testing"
)

// node builds a test vertex.
func node(objectID, versionID string, versionNo int, objectType, title string) Node {
	return Node{ObjectID: objectID, VersionID: versionID, VersionNo: versionNo, ObjectType: objectType, Title: title}
}

// edge builds a test edge.
func edge(relationID, typ string, source, target Node) Edge {
	return Edge{RelationID: relationID, RelationVersionID: relationID + "-v1", RelationType: typ, Source: source, Target: target}
}

// versionIDs extracts the sorted node version ids of a graph.
func versionIDs(g Graph) []string {
	var out []string
	for _, n := range g.Nodes {
		out = append(out, n.VersionID)
	}
	return out
}

// edgeIDs extracts the sorted edge relation ids of a graph.
func edgeIDs(g Graph) []string {
	var out []string
	for _, e := range g.Edges {
		out = append(out, e.RelationID)
	}
	return out
}

// sameStrings compares two string slices without order.
func sameStrings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	seen := map[string]bool{}
	for _, s := range want {
		seen[s] = true
	}
	for _, s := range got {
		if !seen[s] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// TestWalkLineageChain: a derived_from b, b derived_from c — the upstream
// walk of a answers the whole chain ("where did a come from").
func TestWalkLineageChain(t *testing.T) {
	a := node("a", "a-v1", 1, "dataset", "A")
	b := node("b", "b-v1", 1, "dataset", "B")
	c := node("c", "c-v1", 1, "material", "C")
	g := Build([]Edge{
		edge("r1", "derived_from", a, b),
		edge("r2", "derived_from", b, c),
	})
	got := Walk(g, []Node{a}, WalkUpstream, UnlimitedDepth)
	sameStrings(t, versionIDs(got), []string{"a-v1", "b-v1", "c-v1"})
	sameStrings(t, edgeIDs(got), []string{"r1", "r2"})

	// The downstream walk of c is the mirror image.
	got = Walk(g, []Node{c}, WalkDownstream, UnlimitedDepth)
	sameStrings(t, versionIDs(got), []string{"a-v1", "b-v1", "c-v1"})
	sameStrings(t, edgeIDs(got), []string{"r1", "r2"})
}

// TestWalkProducesReversedDirection: produces points origin at the source —
// the lineage of the produced dataset includes the producing experiment,
// and the impact of the experiment includes its output.
func TestWalkProducesReversedDirection(t *testing.T) {
	exp := node("exp", "exp-v1", 1, "experiment", "Synthesis")
	ds := node("ds", "ds-v1", 1, "dataset", "Isotherm")
	g := Build([]Edge{edge("r1", "produces", exp, ds)})

	got := Walk(g, []Node{ds}, WalkUpstream, UnlimitedDepth)
	sameStrings(t, versionIDs(got), []string{"exp-v1", "ds-v1"})
	sameStrings(t, edgeIDs(got), []string{"r1"})

	got = Walk(g, []Node{exp}, WalkDownstream, UnlimitedDepth)
	sameStrings(t, versionIDs(got), []string{"exp-v1", "ds-v1"})
	sameStrings(t, edgeIDs(got), []string{"r1"})

	// And the non-reversed reads are empty: the producer has no upstream
	// input through this edge, the product has no downstream dependent.
	got = Walk(g, []Node{exp}, WalkUpstream, UnlimitedDepth)
	sameStrings(t, versionIDs(got), []string{"exp-v1"})
	sameStrings(t, edgeIDs(got), nil)

	got = Walk(g, []Node{ds}, WalkDownstream, UnlimitedDepth)
	sameStrings(t, versionIDs(got), []string{"ds-v1"})
	sameStrings(t, edgeIDs(got), nil)
}

// TestWalkUsedByReversedDirection: X used_by Y means Y uses X — the
// lineage of Y includes X, the impact of X includes Y.
func TestWalkUsedByReversedDirection(t *testing.T) {
	x := node("x", "x-v1", 1, "dataset", "X")
	y := node("y", "y-v1", 1, "experiment", "Y")
	g := Build([]Edge{edge("r1", "used_by", x, y)})

	got := Walk(g, []Node{y}, WalkUpstream, UnlimitedDepth)
	sameStrings(t, versionIDs(got), []string{"x-v1", "y-v1"})
	got = Walk(g, []Node{x}, WalkDownstream, UnlimitedDepth)
	sameStrings(t, versionIDs(got), []string{"x-v1", "y-v1"})
}

// TestWalkSupersedes: the newer version supersedes the older — the lineage
// of the newer includes the older, the impact of the older includes the
// newer.
func TestWalkSupersedes(t *testing.T) {
	v2 := node("ds", "ds-v2", 2, "dataset", "D v2")
	v1 := node("ds", "ds-v1", 1, "dataset", "D v1")
	g := Build([]Edge{edge("r1", "supersedes", v2, v1)})

	got := Walk(g, []Node{v2}, WalkUpstream, UnlimitedDepth)
	sameStrings(t, versionIDs(got), []string{"ds-v1", "ds-v2"})
	got = Walk(g, []Node{v1}, WalkDownstream, UnlimitedDepth)
	sameStrings(t, versionIDs(got), []string{"ds-v1", "ds-v2"})
}

// TestWalkCycleSafe: a uses b, b uses a — the walk terminates after one
// lap and returns both nodes and both edges.
func TestWalkCycleSafe(t *testing.T) {
	a := node("a", "a-v1", 1, "experiment", "A")
	b := node("b", "b-v1", 1, "protocol", "B")
	g := Build([]Edge{
		edge("r1", "uses", a, b),
		edge("r2", "uses", b, a),
	})
	got := Walk(g, []Node{a}, WalkUpstream, UnlimitedDepth)
	sameStrings(t, versionIDs(got), []string{"a-v1", "b-v1"})
	sameStrings(t, edgeIDs(got), []string{"r1", "r2"})
}

// TestWalkDepthLimit: maxDepth bounds the hop count — depth 0 is the start
// alone, depth 1 the direct neighbors.
func TestWalkDepthLimit(t *testing.T) {
	a := node("a", "a-v1", 1, "dataset", "A")
	b := node("b", "b-v1", 1, "dataset", "B")
	c := node("c", "c-v1", 1, "material", "C")
	g := Build([]Edge{
		edge("r1", "derived_from", a, b),
		edge("r2", "derived_from", b, c),
	})

	got := Walk(g, []Node{a}, WalkUpstream, 0)
	sameStrings(t, versionIDs(got), []string{"a-v1"})
	sameStrings(t, edgeIDs(got), nil)

	got = Walk(g, []Node{a}, WalkUpstream, 1)
	sameStrings(t, versionIDs(got), []string{"a-v1", "b-v1"})
	sameStrings(t, edgeIDs(got), []string{"r1"})

	got = Walk(g, []Node{a}, WalkUpstream, 2)
	sameStrings(t, versionIDs(got), []string{"a-v1", "b-v1", "c-v1"})
	sameStrings(t, edgeIDs(got), []string{"r1", "r2"})
}

// TestWalkSkipsUndeclaredTypes: knowledge and weak edges never enter the
// walk — a supports edge cannot carry provenance, and it stays excluded
// even when both of its endpoints are reachable through declared edges.
func TestWalkSkipsUndeclaredTypes(t *testing.T) {
	claim := node("claim", "claim-v1", 1, "claim", "MOF works")
	exp := node("exp", "exp-v1", 1, "experiment", "Synthesis")
	g := Build([]Edge{edge("r1", "supports", exp, claim)})

	got := Walk(g, []Node{claim}, WalkUpstream, UnlimitedDepth)
	sameStrings(t, versionIDs(got), []string{"claim-v1"})
	sameStrings(t, edgeIDs(got), nil)

	// Both endpoints reachable: the walk reaches the claim through a real
	// provenance edge, but the undeclared supports edge between them must
	// still not appear in the result.
	protocol := node("p", "p-v1", 1, "protocol", "Synthesis protocol")
	g = Build([]Edge{
		edge("r1", "uses", exp, protocol),
		edge("r2", "supports", exp, claim),
	})
	got = Walk(g, []Node{exp}, WalkUpstream, UnlimitedDepth)
	sameStrings(t, versionIDs(got), []string{"exp-v1", "p-v1"})
	sameStrings(t, edgeIDs(got), []string{"r1"})
}

// TestWalkPartOfContainsDuality: the catalog declares contains and part_of
// duals ("contains the target object version as a part" / "is part of the
// target aggregate or composite"), so the same fact recorded either way
// must walk in the same direction — the part is the origin of the
// aggregate in both notations.
func TestWalkPartOfContainsDuality(t *testing.T) {
	aggregate := node("agg", "agg-v1", 1, "dataset", "Aggregate")
	part := node("part", "part-v1", 1, "material", "Part")

	// "aggregate contains part" — origin = the part = target.
	contains := Build([]Edge{edge("r1", "contains", aggregate, part)})
	// "part part_of aggregate" — origin = the part = source.
	partOf := Build([]Edge{edge("r1", "part_of", part, aggregate)})

	for _, g := range map[string]Graph{"contains": contains, "part_of": partOf} {
		// The aggregate's lineage includes its part...
		got := Walk(g, []Node{aggregate}, WalkUpstream, UnlimitedDepth)
		sameStrings(t, versionIDs(got), []string{"agg-v1", "part-v1"})
		sameStrings(t, edgeIDs(got), []string{"r1"})
		// ...the part's impact includes its aggregate...
		got = Walk(g, []Node{part}, WalkDownstream, UnlimitedDepth)
		sameStrings(t, versionIDs(got), []string{"agg-v1", "part-v1"})
		sameStrings(t, edgeIDs(got), []string{"r1"})
		// ...and the non-answers are empty: the part has no upstream input
		// through this edge, the aggregate no downstream dependent.
		got = Walk(g, []Node{part}, WalkUpstream, UnlimitedDepth)
		sameStrings(t, versionIDs(got), []string{"part-v1"})
		sameStrings(t, edgeIDs(got), nil)
		got = Walk(g, []Node{aggregate}, WalkDownstream, UnlimitedDepth)
		sameStrings(t, versionIDs(got), []string{"agg-v1"})
		sameStrings(t, edgeIDs(got), nil)
	}
}

// TestWalkInducedEdges: the result carries every edge between reachable
// nodes, including edges no shortest path uses (the diamond's rim).
func TestWalkInducedEdges(t *testing.T) {
	a := node("a", "a-v1", 1, "dataset", "A")
	b := node("b", "b-v1", 1, "dataset", "B")
	c := node("c", "c-v1", 1, "dataset", "C")
	d := node("d", "d-v1", 1, "material", "D")
	g := Build([]Edge{
		edge("r1", "derived_from", a, b),
		edge("r2", "derived_from", a, c),
		edge("r3", "derived_from", b, d),
		edge("r4", "derived_from", c, d),
		edge("r5", "derived_from", b, c), // the rim: not on any shortest path
	})
	got := Walk(g, []Node{a}, WalkUpstream, UnlimitedDepth)
	sameStrings(t, versionIDs(got), []string{"a-v1", "b-v1", "c-v1", "d-v1"})
	sameStrings(t, edgeIDs(got), []string{"r1", "r2", "r3", "r4", "r5"})
}

// TestWalkStartOutsideGraph: a start the graph does not contain yields the
// start node alone — the answer "this object has no provenance" is honest,
// not empty.
func TestWalkStartOutsideGraph(t *testing.T) {
	lonely := node("lonely", "lonely-v1", 1, "dataset", "Lonely")
	other := node("o", "o-v1", 1, "protocol", "O")
	g := Build([]Edge{edge("r1", "uses", node("x", "x-v1", 1, "experiment", "X"), other)})

	got := Walk(g, []Node{lonely}, WalkUpstream, UnlimitedDepth)
	sameStrings(t, versionIDs(got), []string{"lonely-v1"})
	sameStrings(t, edgeIDs(got), nil)
}

// TestWalkMultipleStarts: several start nodes are seeded together at depth
// 0 (the object-level lineage query walks from more than one pinned
// version when the caller asks for it).
func TestWalkMultipleStarts(t *testing.T) {
	a1 := node("a", "a-v1", 1, "dataset", "A1")
	a2 := node("a", "a-v2", 2, "dataset", "A2")
	b := node("b", "b-v1", 1, "dataset", "B")
	g := Build([]Edge{edge("r1", "derived_from", a2, b)})

	got := Walk(g, []Node{a1, a2}, WalkUpstream, UnlimitedDepth)
	sameStrings(t, versionIDs(got), []string{"a-v1", "a-v2", "b-v1"})
	sameStrings(t, edgeIDs(got), []string{"r1"})
}

// TestBuildDeterministicOrder: the same edge set always builds the same
// sorted graph, whatever the input order.
func TestBuildDeterministicOrder(t *testing.T) {
	a := node("a", "a-v1", 1, "dataset", "A")
	b := node("b", "b-v1", 1, "dataset", "B")
	c := node("c", "c-v2", 2, "material", "C")
	edges := []Edge{
		edge("r2", "derived_from", a, b),
		edge("r1", "uses", b, c),
	}
	g1 := Build(edges)
	g2 := Build([]Edge{edges[1], edges[0]})
	if len(g1.Edges) != 2 || g1.Edges[0].RelationID != "r1" || g1.Edges[1].RelationID != "r2" {
		t.Fatalf("edges not sorted by relation id: %v", edgeIDs(g1))
	}
	if len(g2.Nodes) != len(g1.Nodes) {
		t.Fatalf("node count differs across input orders")
	}
	for i := range g1.Nodes {
		if g1.Nodes[i] != g2.Nodes[i] {
			t.Fatalf("node order differs across input orders: %v vs %v", g1.Nodes, g2.Nodes)
		}
	}
	// The shared endpoint is deduplicated into one node.
	if len(g1.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3 (b is deduplicated)", len(g1.Nodes))
	}
}
