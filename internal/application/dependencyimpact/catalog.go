package dependencyimpact

import (
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/rsg/relationcatalog"
)

// This file is the analysis's entire vocabulary, and it contains no names.
//
// Every dependency type the walk follows, every dependency type the asset
// landing point reads, and the direction of each edge are READS of the
// relation catalog's DependencyInference flag
// (internal/rsg/relationcatalog/catalog.go:42-45: "types that participate in
// dependency / impact analysis: a change upstream must trigger re-analysis
// downstream (docs/19 §3)") — never a list written here.
//
// The reason is not tidiness. `depends_on` and `references` differ in exactly
// one bit, and that bit is the difference between "an upstream change makes
// your work stale" and "someone cited you". If this package spelled either
// name, then the flag would have a second, independent definition, and the
// MUTATION CHECK the acceptance requires — flip the flag in the catalog and
// watch the `references`-only chain appear in the impact set — could pass
// while the real protection was a coincidence of two lists.
//
// The end-to-end consequence of there being no name here: widening or
// narrowing which edges carry impact is a ONE-LINE catalog edit, and the walk,
// the asset landing point, the trigger set's candidate scan and the read
// surface all follow it without a second change.

// Endpoint names one end of a relation edge. It is relationcatalog.Endpoint,
// re-exported rather than redeclared: the direction constants below have to
// be the catalog's own values for DependentTypes to answer correctly, and a
// second string type would be a second vocabulary that could hold a value the
// catalog has never heard of.
type Endpoint = relationcatalog.Endpoint

const (
	// EndpointSource is the relation version's source end,
	// EndpointTarget its target end.
	EndpointSource = relationcatalog.EndpointSource
	EndpointTarget = relationcatalog.EndpointTarget
)

// DependentTypes returns the relation types whose DEPENDENT (downstream) end
// is end, sorted — i.e. the types an analysis follows, split by which end of
// the edge the thing that gets affected sits on.
//
// Two sets rather than one because the direction is per-type and the walk's
// SQL needs them apart: for `depends_on` the dependency is the TARGET (a
// protocol version depends on the target object, catalog.go:78), for
// `used_by` it is the SOURCE. A walk with a single set would have to guess,
// and guessing the wrong end reports the objects UPSTREAM of the change as
// its dependents — an answer that is not merely incomplete but backwards.
//
// A type that carries the flag with no usable direction appears in neither
// set (relationcatalog.DependentTypes), so it is silently not walked — which
// is why the catalog's own test fails on such a declaration rather than
// leaving a caller to discover the hole.
func DependentTypes(end Endpoint) []string {
	return relationcatalog.DependentTypes(end)
}

// AssetDependencyTypes returns the asset_dependencies.dependency_type values
// whose upstream changes trigger re-analysis, sorted: the flag read through
// the package that owns that column's vocabulary rather than through the
// catalog directly, so the asset landing point's types and the asset
// publisher's accepted values (internal/assets.AllDependencyTypes, which
// derives from the same catalog set) are the same set by construction.
//
// A value outside the vocabulary answers false and is therefore not read at
// all: a stored dependency_type the catalog does not know is not a type whose
// upstream changes trigger analysis (assets.DependencyType.
// TriggersImpactAnalysis — false is the fail-closed answer because it claims
// less).
func AssetDependencyTypes() []string {
	all := assets.AllDependencyTypes()
	out := make([]string, 0, len(all))
	for _, t := range all {
		if t.TriggersImpactAnalysis() {
			out = append(out, string(t))
		}
	}
	return out
}
