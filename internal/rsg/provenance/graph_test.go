package provenance

import (
	"encoding/json"
	"testing"
)

// TestBuildEmptyGraphSerializesEmptyArrays: an edge set with no edges (an
// empty project) must serialize both fields as [] — the graph list/JSON
// output is one consistent shape, never "nodes": null next to "edges": [].
func TestBuildEmptyGraphSerializesEmptyArrays(t *testing.T) {
	g := Build(nil)
	if g.Nodes == nil || g.Edges == nil {
		t.Fatalf("Build(nil) produced nil slices: nodes=%v edges=%v", g.Nodes, g.Edges)
	}
	if len(g.Nodes) != 0 || len(g.Edges) != 0 {
		t.Fatalf("Build(nil) = %d nodes / %d edges, want 0/0", len(g.Nodes), len(g.Edges))
	}
	b, err := json.Marshal(g)
	if err != nil {
		t.Fatalf("marshal empty graph: %v", err)
	}
	if string(b) != `{"nodes":[],"edges":[]}` {
		t.Fatalf("empty graph JSON = %s, want {\"nodes\":[],\"edges\":[]}", b)
	}

	// The empty (non-nil) input shapes the same way.
	g = Build([]Edge{})
	if g.Nodes == nil || g.Edges == nil {
		t.Fatalf("Build([]) produced nil slices: nodes=%v edges=%v", g.Nodes, g.Edges)
	}
	b, err = json.Marshal(g)
	if err != nil {
		t.Fatalf("marshal empty graph: %v", err)
	}
	if string(b) != `{"nodes":[],"edges":[]}` {
		t.Fatalf("empty graph JSON = %s, want {\"nodes\":[],\"edges\":[]}", b)
	}
}
