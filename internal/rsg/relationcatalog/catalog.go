package relationcatalog

import (
	"fmt"
	"sort"
	"strings"
)

// Category groups relation types by their scientific role (docs/44).
type Category string

const (
	// CategoryProvenanceStructure: how an object came to be — inputs,
	// processes, protocols, composition (docs/44 "Provenance/structure").
	CategoryProvenanceStructure Category = "provenance_structure"
	// CategoryKnowledge: evidence-shaped relations between claims,
	// questions, hypotheses and results (docs/44 "Knowledge").
	CategoryKnowledge Category = "knowledge"
	// CategoryLineageNetwork: fork/publish/origin lineage across projects
	// and assets (docs/44 "Lineage/network").
	CategoryLineageNetwork Category = "lineage_network"
	// CategoryWeak: related_to, the last-resort unstandardized edge. It
	// must never participate in provenance or dependency derivation
	// (docs/07 §3).
	CategoryWeak Category = "weak"
)

// Entry is one catalog entry: the canonical core relation type, its
// category, a one-line semantics, and the inference flags a namespaced
// extension type would have to declare (docs/44: semantics, participation
// in provenance/dependency/impact).
type Entry struct {
	// Type is the canonical wire value of relation_type.
	Type     string
	Category Category
	// Semantics is the one-line meaning of the edge, from docs/44 and
	// docs/19.
	Semantics string
	// ProvenanceInference marks types that participate in provenance
	// derivation (how things came to be).
	ProvenanceInference bool
	// DependencyInference marks types that participate in dependency /
	// impact analysis: a change upstream must trigger re-analysis
	// downstream (docs/19 §3).
	DependencyInference bool

	// SourceTypes constrains which object types may sit at the source
	// endpoint of this edge; TargetTypes constrains the target endpoint.
	// A nil slice means "unconstrained". Only the end the edge's NAME
	// itself states may be declared — "addresses the target research
	// question" pins the target to research_question but says nothing
	// about who may address it, so the source stays unconstrained
	// (findings address questions too). Most edges declare neither end:
	// they are typed by convention and by the schemas they pin. The two
	// knowledge edges declare their name-pinned targets, and migration
	// 00040 mirrors these declarations in
	// knowledge_relation_endpoint_types.
	SourceTypes []string
	TargetTypes []string

	// ExternalRefTarget marks the types that may point AT an external
	// reference — the citation/dependency pair of docs/19 §3:
	// references (background knowledge; upstream changes only notify)
	// and depends_on (actual input; upstream changes trigger impact
	// analysis). An external reference may never be a relation SOURCE
	// in V1. The database declares the same set
	// (external_reference_relation_types, migration 00045); an
	// integration test pins the two copies together.
	ExternalRefTarget bool

	// Dependency ends: which endpoint of the edge is the DEPENDENCY (the
	// thing that is used) and which is the DEPENDENT (the work that would
	// have to be re-analyzed when the dependency changes). They are
	// declared only for types whose DependencyInference flag is true —
	// the flag says "a change upstream must trigger re-analysis
	// downstream" and these two fields say which end "downstream" is. A
	// type without the flag declares neither, because it has no
	// downstream to name.
	//
	// They are two separate fields rather than one boolean because the
	// flag does not imply a direction: depends_on names its dependency as
	// the TARGET ("uses the target as an actual input") while used_by
	// names it as the SOURCE ("is used by the target") — both carry the
	// flag, and a reader that assumed "the dependency is always the
	// target" would walk used_by backwards and report the wrong objects.
	DependencyEnd Endpoint
	DependentEnd  Endpoint
}

// Endpoint names one end of a relation edge.
type Endpoint string

const (
	// EndpointSource is the relation version's source_object_version_id
	// end, EndpointTarget its target_object_version_id end.
	EndpointSource Endpoint = "source"
	EndpointTarget Endpoint = "target"
)

// DependencyDirection returns which endpoint of typ is the dependency and
// which is the dependent, for a type that participates in dependency
// inference. ok is false for every other type — including a canonical type
// that carries no DependencyInference flag (references: a citation, whose
// upstream changes only notify) and an unknown one (fail closed: an edge
// nobody declared is not an edge to re-analyze over).
func DependencyDirection(typ string) (dependency, dependent Endpoint, ok bool) {
	e, found := lookup[typ]
	if !found || !e.DependencyInference {
		return "", "", false
	}
	if e.DependencyEnd == "" || e.DependentEnd == "" || e.DependencyEnd == e.DependentEnd {
		// A flagged type with no usable declaration names no direction:
		// refusing beats guessing one, and the catalog's own test fails on
		// the declaration rather than leaving callers to pick a side.
		return "", "", false
	}
	return e.DependencyEnd, e.DependentEnd, true
}

// DependentTypes returns the types whose DEPENDENT end is end, sorted: the
// types whose downstream is the edge's source, and the types whose
// downstream is the edge's target, as two disjoint sets. The dependency
// walk needs them apart — one SQL case branch per set, both derived from
// this one declaration — and a type that carried the flag with no
// direction appears in neither.
func DependentTypes(end Endpoint) []string {
	var out []string
	for _, e := range catalog {
		if !e.DependencyInference || e.DependentEnd != end {
			continue
		}
		if e.DependencyEnd == "" || e.DependencyEnd == e.DependentEnd {
			continue
		}
		out = append(out, e.Type)
	}
	sort.Strings(out)
	return out
}

// catalog is the canonical core set (docs/44). related_to comes from
// docs/07 §3: allowed only for weak relations that cannot be standardized,
// and excluded from strong inference.
var catalog = []Entry{
	// Provenance/structure (docs/44).
	{Type: "contains", Category: CategoryProvenanceStructure, Semantics: "contains the target object version as a part", ProvenanceInference: true},
	{Type: "depends_on", Category: CategoryProvenanceStructure, Semantics: "uses the target as an actual input/method/reproducibility dependency; upstream changes trigger impact analysis (docs/19 §3)", ProvenanceInference: true, DependencyInference: true, DependencyEnd: EndpointTarget, DependentEnd: EndpointSource, ExternalRefTarget: true},
	{Type: "derived_from", Category: CategoryProvenanceStructure, Semantics: "was derived or transformed from the target object version", ProvenanceInference: true},
	{Type: "follows_protocol", Category: CategoryProvenanceStructure, Semantics: "was performed following the target protocol", ProvenanceInference: true},
	{Type: "generated_by", Category: CategoryProvenanceStructure, Semantics: "was generated by the target process or activity", ProvenanceInference: true},
	{Type: "parameterized_by", Category: CategoryProvenanceStructure, Semantics: "is parameterized by the target object version", ProvenanceInference: true},
	{Type: "part_of", Category: CategoryProvenanceStructure, Semantics: "is part of the target aggregate or composite", ProvenanceInference: true},
	{Type: "performed_on", Category: CategoryProvenanceStructure, Semantics: "was performed on the target material or sample", ProvenanceInference: true},
	{Type: "produces", Category: CategoryProvenanceStructure, Semantics: "produces the target object version as an output", ProvenanceInference: true},
	{Type: "references", Category: CategoryProvenanceStructure, Semantics: "references the target as background knowledge, not as an input dependency (docs/19 §3)", ProvenanceInference: true, ExternalRefTarget: true},
	{Type: "supersedes", Category: CategoryProvenanceStructure, Semantics: "supersedes the target (older) object version", ProvenanceInference: true},
	{Type: "uses", Category: CategoryProvenanceStructure, Semantics: "uses the target object version as an input or method", ProvenanceInference: true},

	// Knowledge (docs/44).
	{Type: "addresses_question", Category: CategoryKnowledge, Semantics: "addresses the target research question", TargetTypes: []string{"research_question"}},
	{Type: "challenges", Category: CategoryKnowledge, Semantics: "challenges the target claim"},
	{Type: "competes_with", Category: CategoryKnowledge, Semantics: "competes with the target claim or result"},
	{Type: "consistent_with", Category: CategoryKnowledge, Semantics: "is consistent with the target claim"},
	{Type: "contextualizes", Category: CategoryKnowledge, Semantics: "contextualizes the target claim or result"},
	{Type: "contradicts", Category: CategoryKnowledge, Semantics: "contradicts the target claim"},
	{Type: "fails_to_reproduce", Category: CategoryKnowledge, Semantics: "fails to reproduce the target result"},
	{Type: "inconsistent_with", Category: CategoryKnowledge, Semantics: "is inconsistent with the target claim"},
	{Type: "refines", Category: CategoryKnowledge, Semantics: "refines the target claim or model"},
	{Type: "reproduces", Category: CategoryKnowledge, Semantics: "reproduces the target result"},
	{Type: "supports", Category: CategoryKnowledge, Semantics: "supports the target claim"},
	{Type: "tests_hypothesis", Category: CategoryKnowledge, Semantics: "tests the target hypothesis", TargetTypes: []string{"hypothesis"}},
	{Type: "validates", Category: CategoryKnowledge, Semantics: "validates the target claim or result"},

	// Lineage/network (docs/44).
	{Type: "derived_asset_from", Category: CategoryLineageNetwork, Semantics: "is an asset derived from the target asset", ProvenanceInference: true},
	{Type: "forked_from", Category: CategoryLineageNetwork, Semantics: "was forked from the target lineage", ProvenanceInference: true},
	{Type: "originates_from", Category: CategoryLineageNetwork, Semantics: "originates from the target external source", ProvenanceInference: true},
	{Type: "published_from", Category: CategoryLineageNetwork, Semantics: "was published from the target state", ProvenanceInference: true},
	{Type: "used_by", Category: CategoryLineageNetwork, Semantics: "is used by the target (reverse projection of a dependency edge)", ProvenanceInference: true, DependencyInference: true, DependencyEnd: EndpointSource, DependentEnd: EndpointTarget},

	// Weak (docs/07 §3).
	{Type: "related_to", Category: CategoryWeak, Semantics: "weak unstandardized relation; must not participate in provenance or dependency inference (docs/07 §3)"},
}

// lookup caches the catalog by type name.
var lookup = func() map[string]Entry {
	m := make(map[string]Entry, len(catalog))
	for _, e := range catalog {
		m[e.Type] = e
	}
	return m
}()

// Lookup returns the catalog entry for typ, or ok=false when typ is not a
// canonical core relation type. Namespaced extension types
// ("materials:synthesized_from") are not registered in V1 and therefore
// fail lookup: docs/44 requires them to be declared (semantics, endpoints,
// inference participation) before use, and V1 has no declaration mechanism
// yet.
func Lookup(typ string) (Entry, bool) {
	e, ok := lookup[typ]
	return e, ok
}

// Valid reports whether typ is a canonical core relation type.
func Valid(typ string) bool {
	_, ok := lookup[typ]
	return ok
}

// EndpointsValid reports whether a source endpoint of type sourceType and
// a target endpoint of type targetType satisfy the endpoint-type
// declaration of the relation type typ. Only the end an edge's NAME
// states may be declared (see Entry.SourceTypes/TargetTypes); a nil
// declaration accepts every type for that end, and an unknown relation
// type is not valid by definition, so it answers false with an
// explanation. The database guard (migration 00040,
// knowledge_relation_endpoint_types) enforces the same rule for the two
// knowledge edges; this is the Go-side declaration the guard mirrors.
func EndpointsValid(typ, sourceType, targetType string) (bool, string) {
	e, ok := lookup[typ]
	if !ok {
		return false, fmt.Sprintf("%s is not a canonical relation type", typ)
	}
	if len(e.SourceTypes) > 0 && !contains(e.SourceTypes, sourceType) {
		return false, fmt.Sprintf("%s: source object type %s is not allowed (allowed: %s)", typ, sourceType, strings.Join(e.SourceTypes, ", "))
	}
	if len(e.TargetTypes) > 0 && !contains(e.TargetTypes, targetType) {
		return false, fmt.Sprintf("%s: target object type %s is not allowed (allowed: %s)", typ, targetType, strings.Join(e.TargetTypes, ", "))
	}
	return true, ""
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// Types returns the whole catalog sorted by type name.
func Types() []Entry {
	out := make([]Entry, len(catalog))
	copy(out, catalog)
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// TypesOfCategory returns the type names of one category, sorted.
func TypesOfCategory(cat Category) []string {
	var out []string
	for _, e := range catalog {
		if e.Category == cat {
			out = append(out, e.Type)
		}
	}
	sort.Strings(out)
	return out
}

// ProvenanceTypes returns every type that participates in provenance
// inference, sorted.
func ProvenanceTypes() []string {
	var out []string
	for _, e := range catalog {
		if e.ProvenanceInference {
			out = append(out, e.Type)
		}
	}
	sort.Strings(out)
	return out
}

// DependencyTypes returns every type that participates in dependency /
// impact inference (docs/19 §3: upstream changes trigger re-analysis),
// sorted.
func DependencyTypes() []string {
	var out []string
	for _, e := range catalog {
		if e.DependencyInference {
			out = append(out, e.Type)
		}
	}
	sort.Strings(out)
	return out
}

// ExternalRefTargetTypes returns every type that may point AT an external
// reference — the citation/dependency pair of docs/19 §3 (references,
// depends_on), sorted. The database declares the same set
// (external_reference_relation_types, migration 00045); an integration
// test pins the two copies together.
func ExternalRefTargetTypes() []string {
	var out []string
	for _, e := range catalog {
		if e.ExternalRefTarget {
			out = append(out, e.Type)
		}
	}
	sort.Strings(out)
	return out
}
