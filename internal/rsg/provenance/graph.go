package provenance

import "sort"

// Node is one vertex of the provenance graph: a pinned scientific object
// version with its display identity resolved. The same version appearing as
// the endpoint of several edges is one node (deduplicated by VersionID).
type Node struct {
	// ObjectID is the version's container object.
	ObjectID string `json:"object_id"`
	// VersionID is the pinned version's own id (edges are version-pinned,
	// docs/07 §3).
	VersionID string `json:"version_id"`
	// VersionNo is the 1-based position in the object's version log.
	VersionNo int `json:"version_no"`
	// ObjectType is the container object's scientific type.
	ObjectType string `json:"object_type"`
	// Title is the version's display title.
	Title string `json:"title"`
}

// Edge is one provenance edge: a relation version whose relation_type is a
// provenance-category type, with both endpoint nodes resolved. The edge's
// own row identity (relation + version) rides along so the wire can pin
// exactly which append-only version an edge came from.
type Edge struct {
	RelationID string `json:"relation_id"`
	// RelationVersionID is the append-only relation_versions row the edge
	// projects.
	RelationVersionID string `json:"relation_version_id"`
	RelationType      string `json:"relation_type"`
	Source            Node   `json:"source"`
	Target            Node   `json:"target"`
}

// Graph is a provenance subgraph rendered for the wire: the nodes and the
// edges, both in deterministic order (nodes by object then version, edges
// by relation id) so the same data always serializes identically.
type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// Build assembles a graph from an edge set: the nodes are the endpoint
// versions the edges pin, deduplicated by version id and sorted; the edges
// are kept in place but sorted by relation id. The caller filters the edge
// set — the projection (migration 00043) only ever yields
// provenance-category types, and the walks skip anything undeclared
// regardless. Nodes and edges are always non-nil, so an empty edge set
// serializes as "nodes": [], "edges": [] — never "nodes": null.
func Build(edges []Edge) Graph {
	g := Graph{Nodes: make([]Node, 0, len(edges)*2), Edges: sortedEdges(edges)}
	seen := make(map[string]struct{}, len(g.Edges)*2)
	for _, e := range g.Edges {
		for _, n := range []Node{e.Source, e.Target} {
			if _, ok := seen[n.VersionID]; ok {
				continue
			}
			seen[n.VersionID] = struct{}{}
			g.Nodes = append(g.Nodes, n)
		}
	}
	sortNodes(g.Nodes)
	return g
}

// sortNodes orders nodes by object id then version number — the
// deterministic wire order.
func sortNodes(nodes []Node) {
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].ObjectID != nodes[j].ObjectID {
			return nodes[i].ObjectID < nodes[j].ObjectID
		}
		return nodes[i].VersionNo < nodes[j].VersionNo
	})
}

// sortedEdges returns the edge set ordered by relation id (then version id,
// for a deterministic tiebreak when two edges of one relation are passed).
func sortedEdges(edges []Edge) []Edge {
	out := make([]Edge, len(edges))
	copy(out, edges)
	sort.Slice(out, func(i, j int) bool {
		if out[i].RelationID != out[j].RelationID {
			return out[i].RelationID < out[j].RelationID
		}
		return out[i].RelationVersionID < out[j].RelationVersionID
	})
	return out
}
