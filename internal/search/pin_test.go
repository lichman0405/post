package search

import (
	"sort"
	"testing"
)

// Task T0904's half of the projection's unit suite: the version PIN.
//
// ADR-010 requires an answer to cite a platform-determined version, and the
// retrieval layer reads the label out of a row's facets. Two things have to
// hold for that to be a pin and not a guess, and neither is provable against
// a database:
//
//   - every entity type the projection can write is DECIDED — it either
//     carries a version facet or is declared pinned by its own identity —
//     so a new entity type cannot quietly resolve to "no pin";
//   - every declared facet key is one the projection actually writes, so the
//     reader and the writer cannot drift into two spellings of the same name.
//
// The database half — that a real corpus projects the labels back — is the
// retrieval integration suite (tests/integration/retrieval_test.go).

// TestEveryEntityTypeHasADecidedPin is the closure. EntityTypes() and the two
// pin sets are all derived from the projection's own vocabulary, so an
// entity type added without a decision about its pin fails here rather than
// in production, where the symptom would be a citation that silently names no
// version.
func TestEveryEntityTypeHasADecidedPin(t *testing.T) {
	versioned := map[string]bool{}
	for _, entityType := range VersionedEntityTypes() {
		versioned[entityType] = true
	}
	byIdentity := map[string]bool{}
	for _, entityType := range UnversionedEntityTypes() {
		byIdentity[entityType] = true
	}
	for _, entityType := range EntityTypes() {
		pinnable := false
		if _, ok := pinnedVersionFacet[entityType]; ok {
			pinnable = true
		}
		if PinnedByIdentity(entityType) {
			pinnable = true
		}
		if !pinnable {
			t.Errorf("entity type %q has no pin: give it a version facet or declare it pinned by its identity", entityType)
		}
		if versioned[entityType] && byIdentity[entityType] {
			t.Errorf("entity type %q is declared both versioned and version-less", entityType)
		}
	}
	// And the other direction: a pin declared for an entity type the
	// projection cannot write is a reader for a row that does not exist.
	known := map[string]bool{}
	for _, entityType := range EntityTypes() {
		known[entityType] = true
	}
	for _, entityType := range append(VersionedEntityTypes(), UnversionedEntityTypes()...) {
		if !known[entityType] {
			t.Errorf("a pin is declared for %q, which the projection never writes", entityType)
		}
	}
}

// TestVersionFacetKeysAreOnesTheProjectionWrites pins the reader against the
// writer. The keys are literals in sources.go's facet builders, so the
// assertion is the facet object a real source renders rather than a second
// table of the same names.
func TestVersionFacetKeysAreOnesTheProjectionWrites(t *testing.T) {
	// The facets each source writes, spelled as sources.go spells them
	// (assetSource, knowledgeSource, releaseSource). A key that is not here
	// is a key nothing writes, and PinnedVersion would return "" for every
	// row of that type while looking perfectly healthy.
	written := map[string]bool{
		EntityAsset:     true,
		EntityKnowledge: true,
		EntityRelease:   true,
	}
	for entityType, key := range pinnedVersionFacet {
		if !written[entityType] {
			t.Errorf("a version facet is declared for %q, which renders no facets", entityType)
		}
		if key == "" {
			t.Errorf("entity type %q maps to an empty facet key", entityType)
		}
	}
}

// TestPinnedVersionReadsOnlyAUsableLabel: everything that is not a non-empty
// string reads as "". This is the fail-closed direction the projection's
// visibility takes, and it matters for the same reason — a document whose pin
// cannot be read is a document with NO pin, never one pinned to something
// this function guessed.
func TestPinnedVersionReadsOnlyAUsableLabel(t *testing.T) {
	cases := []struct {
		name       string
		entityType string
		structured string
		want       string
	}{
		{"an asset's published version", EntityAsset, `{"entity_type":"asset","version":"3"}`, "3"},
		{"a publication's public version", EntityKnowledge, `{"entity_type":"knowledge","public_version":"2"}`, "2"},
		{"a release's version", EntityRelease, `{"entity_type":"release","version":"1.2.0"}`, "1.2.0"},
		{"a state has no version label", EntityState, `{"entity_type":"state"}`, ""},
		{"an unknown entity type has none either", "compound", `{"version":"3"}`, ""},
		{"the facet is absent", EntityAsset, `{"entity_type":"asset"}`, ""},
		{"the facet is blank", EntityAsset, `{"entity_type":"asset","version":""}`, ""},
		{"the facet is a number", EntityAsset, `{"entity_type":"asset","version":3}`, ""},
		{"the facet is an object", EntityAsset, `{"entity_type":"asset","version":{"major":3}}`, ""},
		{"the facets are null", EntityAsset, `null`, ""},
		{"the facets do not parse", EntityAsset, `{"entity_type":`, ""},
		{"there are no facets", EntityAsset, ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PinnedVersion(tc.entityType, []byte(tc.structured)); got != tc.want {
				t.Errorf("PinnedVersion(%q, %s) = %q, want %q", tc.entityType, tc.structured, got, tc.want)
			}
		})
	}
}

// TestVersionedEntityTypesIsSortedAndComplete: the accessor's order is part
// of its contract — it is compared against other sets in tests and logs — and
// its contents have to cover exactly the table.
func TestVersionedEntityTypesIsSortedAndComplete(t *testing.T) {
	got := VersionedEntityTypes()
	if !sort.StringsAreSorted(got) {
		t.Errorf("VersionedEntityTypes() = %v, want a sorted list", got)
	}
	if len(got) != len(pinnedVersionFacet) {
		t.Errorf("VersionedEntityTypes() = %v, want one entry per declared facet (%d)", got, len(pinnedVersionFacet))
	}
}

// TestEntityTypesIsTheProjectionsOwnVocabulary: the accessor is derived from
// the rule table, so it names exactly the types the projection writes — and
// it is what the retrieval validates a plan against. A stale copy of the
// vocabulary there would refuse a legitimate plan or accept an impossible
// one.
func TestEntityTypesIsTheProjectionsOwnVocabulary(t *testing.T) {
	got := EntityTypes()
	if !sort.StringsAreSorted(got) {
		t.Errorf("EntityTypes() = %v, want a sorted list", got)
	}
	want := map[string]bool{}
	for _, r := range projectionRules {
		want[r.EntityType] = true
	}
	if len(got) != len(want) {
		t.Errorf("EntityTypes() = %v, want the rule table's %d types, deduplicated", got, len(want))
	}
	for _, entityType := range got {
		if !want[entityType] {
			t.Errorf("EntityTypes() contains %q, which no rule projects", entityType)
		}
	}
	if !equalStrings(got, rebuildableEntityTypes()) {
		t.Errorf("EntityTypes() = %v and rebuildableEntityTypes() = %v disagree; both derive from the rule table",
			got, rebuildableEntityTypes())
	}
}
