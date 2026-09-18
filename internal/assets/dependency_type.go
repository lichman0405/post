package assets

import (
	"sort"

	"github.com/lichman0405/post/internal/rsg/relationcatalog"
)

// DependencyType is HOW one project uses one published asset version: the
// value of asset_dependencies.dependency_type (migration 00010 line 58).
// The table has recorded the column since 00010 and the column has never
// had a CHECK — deliberately, and this file is where the vocabulary lives
// instead (see "Where the vocabulary lives" below).
//
// # The two values, and where they come from
//
// The vocabulary is NOT invented here. docs/19 §3 draws the distinction
// verbatim — "Cites: 背景/知识引用；上游变化只提示" against "depends_on:
// 实际输入/方法/复现依赖；上游变更触发 impact analysis" — and the relation
// catalog already carries it as the only pair of relation types that may
// point at an external reference:
//
//	depends_on  "uses the target as an actual input/method/reproducibility
//	             dependency; upstream changes trigger impact analysis"
//	             (catalog.go:78) — DependencyInference: true
//	references  "references the target as background knowledge, not as an
//	             input dependency" (catalog.go:86) — no DependencyInference
//
// catalog.go:42-45 defines the flag: "DependencyInference marks types that
// participate in dependency / impact analysis: a change upstream must
// trigger re-analysis downstream (docs/19 §3)". So the distinction this
// table's column spells is exactly the distinction the flag encodes:
// depends_on is the value that carries the flag, references is the value
// that does not.
//
// The same pair is what docs/11 §5 separates on the asset side — "Reference/
// Use：引用固定 Asset Version，不修改" against "Dependency：项目复现/运行
// 所需，参与 impact analysis" — which is why a project's use of a pinned
// version is either a citation or a dependency and not something else. The
// repository has already used these two spellings, and only these two, for
// this distinction in every place it appears (internal/rsg/relationcatalog,
// migration 00045's external_reference_relation_types); a third spelling
// would be a second word for one concept.
//
// # Where the vocabulary lives (and why there is no CHECK)
//
// In Go, here — not in the database. That is the convention this repository
// has declared four times over: 00064_asset_pid_origin.sql:41-45 writes it
// out ("duplicating that shape as a CHECK here would mean two definitions
// of the origin vocabulary drifting apart"), 00066:21-30, 00067:22-34 and
// 00045:74-79 follow it, and the origin vocabulary of 00064's own column is
// declared in internal/assets/origin.go. dependency_type has carried no
// CHECK since 00010 and this task adds none: the column stays text NOT NULL
// and this value is the one definition of what it may hold.
//
// # The values are read from the catalog, not copied from it
//
// AllDependencyTypes derives the set from relationcatalog, so there is ONE
// definition of which names are admissible rather than two that must be kept
// in step. A test in this package pins the derived set to the two names
// above, so a catalog change that widened or renamed the pair fails loudly
// in this package instead of silently widening what a publish records.
type DependencyType string

const (
	// DependencyTypeReferences is a CITATION: the project references the
	// version as background knowledge. Upstream changes only notify
	// (docs/19 §3, catalog.go:86); a change to it does NOT trigger impact
	// analysis, which is what the missing DependencyInference flag says.
	DependencyTypeReferences DependencyType = "references"

	// DependencyTypeDependsOn is a DEPENDENCY: the project uses the version
	// as an actual input, method or reproducibility dependency. Upstream
	// changes trigger impact analysis (docs/19 §3, catalog.go:78), which is
	// what the DependencyInference flag says.
	DependencyTypeDependsOn DependencyType = "depends_on"
)

// AllDependencyTypes returns the vocabulary of asset_dependencies.
// dependency_type, sorted: the citation/dependency pair of docs/19 §3, as
// the relation catalog declares it (relationcatalog.ExternalRefTargetTypes
// is documented as exactly that pair, and a test in the catalog package
// pins it to the two names).
//
// It is derived rather than listed so that the catalog stays the single
// definition of the pair; the names this package writes are the constants
// above, and TestDependencyTypeVocabulary pins the two together.
func AllDependencyTypes() []DependencyType {
	types := relationcatalog.ExternalRefTargetTypes()
	out := make([]DependencyType, 0, len(types))
	for _, t := range types {
		out = append(out, DependencyType(t))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ParseDependencyType reads a stored or declared dependency_type. ok is
// false for anything outside the vocabulary — including the empty string,
// which is not a third value.
func ParseDependencyType(s string) (DependencyType, bool) {
	t := DependencyType(s)
	return t, t.Valid()
}

// Valid reports whether t is in the vocabulary of AllDependencyTypes. A
// stored row naming something else is a repository state this platform
// cannot produce (the only writer is the publish path, which takes its
// value from this vocabulary): the readers render the stored string
// verbatim rather than refusing the row, and this method is what the writer
// checks before it writes.
func (t DependencyType) Valid() bool {
	for _, known := range AllDependencyTypes() {
		if t == known {
			return true
		}
	}
	return false
}

// TriggersImpactAnalysis reports whether an upstream change to the named
// version must trigger re-analysis of the projects that use it: true for
// depends_on, false for references (docs/19 §3, docs/11 §5, catalog.go:42-45).
//
// The answer is the catalog's own DependencyInference flag for this value,
// read through Lookup — the same flag the provenance/dependency surfaces
// read — so "which of the two values is the impact-analysis one" has one
// answer in this tree. A value outside the vocabulary answers false: a type
// the catalog does not know is not a type whose upstream changes trigger
// re-analysis, and false is the fail-closed answer (it claims less).
func (t DependencyType) TriggersImpactAnalysis() bool {
	entry, ok := relationcatalog.Lookup(string(t))
	return ok && entry.DependencyInference
}
