package provenance

// OriginSide is which endpoint of a provenance edge is the origin side —
// the "where it came from" endpoint. Most provenance relations point at
// the target: "X derived_from Y" means X came from Y. Three point at the
// source: "X produces Y" means Y came from X (X is the producer);
// "X used_by Y" — the declared reverse projection of a dependency edge —
// means Y came from X (Y uses X); and "X part_of Y" means Y came from X
// (X is a part of the aggregate Y — the dual of "Y contains X", which
// points origin at the target part: an aggregate is assembled from its
// parts, so the part is the origin in both notations).
type OriginSide int

const (
	// OriginIsTarget: the edge's source came from its target. Lineage
	// walks source → target; impact walks target → source.
	OriginIsTarget OriginSide = iota
	// OriginIsSource: the edge's target came from its source. Lineage
	// walks target → source; impact walks source → target.
	OriginIsSource
)

// originSide pins the direction of every provenance-category relation type
// (docs/44 semantics, docs/10 §2). The set must equal
// relationcatalog.ProvenanceTypes() exactly — TestDirectionMatchesCatalog
// asserts the two copies both ways, so a catalog change cannot drift from
// the walk (and the migration's SQL copy is pinned by the integration
// test's catalog cross-check).
var originSide = map[string]OriginSide{
	// Provenance/structure (docs/44).
	"contains":         OriginIsTarget, // origin = the contained part = target
	"depends_on":       OriginIsTarget,
	"derived_from":     OriginIsTarget,
	"follows_protocol": OriginIsTarget,
	"generated_by":     OriginIsTarget,
	"parameterized_by": OriginIsTarget,
	"part_of":          OriginIsSource, // origin = the part = source (dual of contains)
	"performed_on":     OriginIsTarget,
	"produces":         OriginIsSource,
	"references":       OriginIsTarget,
	"supersedes":       OriginIsTarget,
	"uses":             OriginIsTarget,
	// Lineage/network (docs/44).
	"derived_asset_from": OriginIsTarget,
	"forked_from":        OriginIsTarget,
	"originates_from":    OriginIsTarget,
	"published_from":     OriginIsTarget,
	"used_by":            OriginIsSource,
}

// originOf splits an edge into its origin endpoint and the endpoint derived
// from it. ok is false when the type has no declared origin side — the
// knowledge and weak types, or a future type nobody declared (docs/44: an
// undeclared relation never enters strong inference). Walks skip such edges
// rather than guess a direction.
func originOf(e Edge) (origin, derived Node, ok bool) {
	side, declared := originSide[e.RelationType]
	if !declared {
		return Node{}, Node{}, false
	}
	if side == OriginIsSource {
		return e.Source, e.Target, true
	}
	return e.Target, e.Source, true
}
