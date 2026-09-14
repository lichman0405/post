// Package provenance owns the provenance graph read model (T0505, docs/10):
// a graph over pinned scientific object versions whose edges are the
// provenance-category relation versions (docs/44: uses, produces,
// derived_from, follows_protocol, ...), and the two walks over it — lineage,
// the upstream question "where did this come from", and impact, the
// downstream question "what depends on this".
//
// The package is pure: it builds and walks graphs in memory and never reads
// or writes storage. The persistence side is the rebuildable
// provenance_edges projection (infra/migrations/00043), whose rows the
// transport assembles into the edge set here; the walks then run entirely
// in-process, deterministic and cycle-safe.
//
// docs/10 §1 is the boundary the package enforces: the provenance graph and
// the evidence graph are distinct. Only relation types with a declared
// origin direction participate in the walks — that is exactly
// relationcatalog.ProvenanceTypes() (docs/44); knowledge types
// (supports/contradicts/...) and the weak related_to never do, and an
// undeclared type is skipped rather than guessed (docs/44: an undeclared
// custom relation does not enter strong inference).
package provenance
