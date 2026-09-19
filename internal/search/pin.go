package search

import (
	"encoding/json"
	"sort"
)

// The version pin of a projected document (T0904).
//
// ADR-010 requires an answer to cite platform-determined entity VERSIONS,
// and examples/search-answer.example.json renders such a citation
// ("CLM-DEMO-001@2"). A recall candidate therefore has to be able to say
// which version it names, and for a document the answer is a facet the
// projection already writes: each source renders the version of the entity
// the network currently sees into structured (sources.go — an asset's
// latest PUBLISHED version, a publication's public_version, a release's
// version), because that facet is what makes "which version is this
// document about" answerable without re-reading the source row.
//
// The table below is the ONE place that maps an entity type to its facet
// key, and it lives here rather than in the reader for the reason the facets
// themselves live here: a reader that knew the keys by heart would be a
// second, silent copy of the shape sources.go writes. It is pinned
// end-to-end by the retrieval integration suite, which projects a real
// corpus and reads the labels back.
//
// state is deliberately absent. A project state has no version label: it is
// addressed by its row id, it carries no version column (00004), and its
// content is a commit message the commit already fixes. Its immutability is
// what makes the row id a sufficient pin — a versioned label would be a
// second name for the same thing, not more information. PinnedVersion
// returns "" for it, and a candidate with no version renders without the
// "@version" suffix rather than with an invented one.
var pinnedVersionFacet = map[string]string{
	EntityAsset:     "version",
	EntityKnowledge: "public_version",
	EntityRelease:   "version",
}

// unversionedEntityTypes are the entity types that are pinned by their own
// identity rather than by a version label. It has exactly one member and the
// reason is on pinnedVersionFacet above: a project state is addressed by its
// row id, carries no version column (00004), and its content is a commit
// message the commit already fixes, so the id IS the pin.
//
// It exists as a declared SET rather than as a comment so that "every entity
// type either has a version facet or is declared version-less" is a property
// the unit suite can assert, over the vocabulary both sets are derived from
// (EntityTypes). A new entity type added to the projection without a decision
// about its pin fails that closure instead of silently resolving to "no pin"
// — which is what a reader that only knew the versioned table would do.
var unversionedEntityTypes = map[string]bool{
	EntityState: true,
}

// PinnedVersion reads a projected document's version label out of its
// structured facets, or returns "" when the entity type has no version
// label (state) or the facet is missing, empty or not a string.
//
// Everything that is not a usable label reads as "": an absent facet, a
// blank one, a number, a nested object. That is the same fail-closed
// direction the projection's visibility takes — a document whose pin cannot
// be read is a document with no pin, never a document pinned to something
// this function guessed. Malformed JSON reads as "" too: the facets are
// written by structuredFacets and are always an object, so bytes that do not
// parse were not written by it.
func PinnedVersion(entityType string, structured []byte) string {
	key, ok := pinnedVersionFacet[entityType]
	if !ok || len(structured) == 0 {
		return ""
	}
	var facets map[string]any
	if err := json.Unmarshal(structured, &facets); err != nil {
		return ""
	}
	label, _ := facets[key].(string)
	return label
}

// VersionedEntityTypes returns the entity types a projected document can
// carry a version pin for, sorted. It is what "every entity type either has
// a pin or is declared version-less" is asserted against.
func VersionedEntityTypes() []string {
	out := make([]string, 0, len(pinnedVersionFacet))
	for entityType := range pinnedVersionFacet {
		out = append(out, entityType)
	}
	sort.Strings(out)
	return out
}

// UnversionedEntityTypes returns the entity types a projected document is
// pinned for without a version label, sorted (see
// unversionedEntityTypes).
func UnversionedEntityTypes() []string {
	out := make([]string, 0, len(unversionedEntityTypes))
	for entityType := range unversionedEntityTypes {
		out = append(out, entityType)
	}
	sort.Strings(out)
	return out
}

// PinnedByIdentity reports whether a document of this entity type is pinned
// by its own identity: it has no version label, and its identity is
// immutable, so citing it names one version of one thing.
func PinnedByIdentity(entityType string) bool { return unversionedEntityTypes[entityType] }
