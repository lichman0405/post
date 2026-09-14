package provenance

// WalkDirection selects the question a walk answers.
type WalkDirection int

const (
	// WalkUpstream walks lineage: from the start node toward its origins —
	// the inputs, protocols, samples and producers the start came from
	// ("可回答 Dataset 从哪来").
	WalkUpstream WalkDirection = iota
	// WalkDownstream walks impact: from the start node toward the things
	// derived from it — the dependents an upstream change would affect.
	WalkDownstream
)

// String returns the wire token of the direction (the lineage endpoint's
// direction query parameter).
func (d WalkDirection) String() string {
	switch d {
	case WalkDownstream:
		return "downstream"
	default:
		return "upstream"
	}
}

// ParseWalkDirection maps the wire token to a direction. ok is false for
// anything else — callers answer a validation error.
func ParseWalkDirection(s string) (WalkDirection, bool) {
	switch s {
	case "", "upstream":
		return WalkUpstream, true
	case "downstream":
		return WalkDownstream, true
	}
	return WalkUpstream, false
}

// UnlimitedDepth is the maxDepth value meaning "no hop limit": the walk is
// bounded by the graph itself (and the visited set, which makes cycles
// safe).
const UnlimitedDepth = -1

// Walk returns the subgraph reachable from starts by following provenance
// edges in dir, up to maxDepth edge hops away from a start node
// (maxDepth == UnlimitedDepth walks the whole reachable graph; maxDepth 0
// returns the start nodes alone).
//
// The result is an induced subgraph over the declared provenance edges:
// every node reachable within the depth limit, plus every edge whose type
// declares an origin side and whose two endpoints are both reachable —
// including edges between two reachable nodes that no shortest path uses.
// Nodes and edges are in Build's deterministic order.
//
// The walk is cycle-safe by construction: a node is expanded at most once
// (the first time it is reached, at its minimal depth), so a cycle
// terminates after one lap. Edges whose type has no declared origin side
// are skipped entirely — they never extend reachability and never appear
// in the result (docs/44: undeclared relations never enter strong
// inference).
func Walk(g Graph, starts []Node, dir WalkDirection, maxDepth int) Graph {
	// next maps one endpoint of a traversable edge to the node reached
	// from it: for the upstream walk that is the origin side reached from
	// the derived endpoint; for the downstream walk the derived side
	// reached from the origin endpoint. Edges without a declared origin
	// side never enter either map.
	next := make(map[string][]Node)
	for _, e := range g.Edges {
		origin, derived, ok := originOf(e)
		if !ok {
			continue
		}
		if dir == WalkUpstream {
			next[derived.VersionID] = append(next[derived.VersionID], origin)
		} else {
			next[origin.VersionID] = append(next[origin.VersionID], derived)
		}
	}

	reachable := make(map[string]bool)
	var queue []Node
	var depth []int
	for _, n := range starts {
		if !reachable[n.VersionID] {
			reachable[n.VersionID] = true
			queue = append(queue, n)
			depth = append(depth, 0)
		}
	}
	for i := 0; i < len(queue); i++ {
		if maxDepth != UnlimitedDepth && depth[i] >= maxDepth {
			continue // at the depth limit: do not expand this node further
		}
		for _, n := range next[queue[i].VersionID] {
			if reachable[n.VersionID] {
				continue
			}
			reachable[n.VersionID] = true
			queue = append(queue, n)
			depth = append(depth, depth[i]+1)
		}
	}

	// The result is an induced subgraph over the declared provenance edges:
	// every reachable node (the queue, which carries the start nodes even
	// when nothing is reachable from them), plus every declared-origin edge
	// whose two endpoints are both reachable. Undeclared types never appear
	// in the result, even between two reachable nodes — they are skipped
	// entirely, exactly as the doc contract states.
	edges := make([]Edge, 0, len(g.Edges))
	for _, e := range g.Edges {
		if _, _, declared := originOf(e); !declared {
			continue
		}
		if reachable[e.Source.VersionID] && reachable[e.Target.VersionID] {
			edges = append(edges, e)
		}
	}
	nodes := make([]Node, len(queue))
	copy(nodes, queue)
	sortNodes(nodes)
	return Graph{Nodes: nodes, Edges: sortedEdges(edges)}
}
