package assets

import (
	"testing"

	"github.com/lichman0405/post/internal/rsg/relationcatalog"
)

// Task T0707 acceptance criterion 1 and 7: the vocabulary of
// asset_dependencies.dependency_type is exactly the citation/dependency pair
// the relation catalog declares — two names, in Go, with no third spelling
// invented here and no CHECK in the database.
//
// The tests below are negative where they can be: the point is not that
// "depends_on" and "references" are strings this package knows, it is that
// they are the SAME pair the catalog defines, in both directions — nothing
// added here that the catalog does not have, and nothing the catalog has
// dropped here. A copy of the pair would pass the first assertion and fail
// the second the moment the catalog changed.

// TestDependencyTypeVocabularyIsTheCatalogPair pins the derived vocabulary to
// the two names, and pins those two names to the catalog's own external-ref
// pair (catalog.go:78 depends_on, :86 references) rather than to a literal
// written out twice.
func TestDependencyTypeVocabularyIsTheCatalogPair(t *testing.T) {
	got := AllDependencyTypes()
	want := []DependencyType{DependencyTypeDependsOn, DependencyTypeReferences}
	if len(got) != len(want) {
		t.Fatalf("AllDependencyTypes() = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AllDependencyTypes()[%d] = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}

	// The constants themselves are the catalog's spellings, not a second
	// set of words for the same concept. The catalog is the repository's
	// one definition of the dependency/citation distinction (docs/19 §3).
	for _, typ := range want {
		if !relationcatalog.Valid(string(typ)) {
			t.Errorf("%q is in this package's vocabulary but not in the relation catalog", typ)
		}
	}
	// And no value the catalog declares for external refs is missing here:
	// a catalog entry this package did not carry would be a stored value a
	// publish could never write.
	for _, typ := range relationcatalog.ExternalRefTargetTypes() {
		if !DependencyType(typ).Valid() {
			t.Errorf("the catalog declares the external-ref type %q, which this package's vocabulary does not carry", typ)
		}
	}
}

// TestDependencyTypeImpactAnalysisSplitIsTheCatalogFlag: the flag is the
// whole reason the pair exists — docs/19 §3 makes depends_on trigger impact
// analysis and references only notify. The test asserts both values against
// the catalog's DependencyInference flag AND that the two disagree, so a
// catalog edit that set the flag on both (or neither) fails here instead of
// silently turning every citation into a dependency.
func TestDependencyTypeImpactAnalysisSplitIsTheCatalogFlag(t *testing.T) {
	for _, typ := range AllDependencyTypes() {
		entry, ok := relationcatalog.Lookup(string(typ))
		if !ok {
			t.Fatalf("%q is not a catalog entry", typ)
		}
		if got := typ.TriggersImpactAnalysis(); got != entry.DependencyInference {
			t.Errorf("TriggersImpactAnalysis(%q) = %v, catalog DependencyInference = %v", typ, got, entry.DependencyInference)
		}
	}
	if DependencyTypeDependsOn.TriggersImpactAnalysis() == DependencyTypeReferences.TriggersImpactAnalysis() {
		t.Fatal("depends_on and references answer the same about impact analysis; the distinction docs/19 §3 draws is gone")
	}
	if !DependencyTypeDependsOn.TriggersImpactAnalysis() {
		t.Error("depends_on does not trigger impact analysis (docs/19 §3, catalog.go:78)")
	}
	if DependencyTypeReferences.TriggersImpactAnalysis() {
		t.Error("references triggers impact analysis (docs/19 §3, catalog.go:86: upstream changes only notify)")
	}
}

// TestDependencyTypeUnknownValueClaimsNothing: a value outside the vocabulary
// answers false rather than guessing. False is the fail-closed answer — it
// claims less. The empty string is not a third value.
func TestDependencyTypeUnknownValueClaimsNothing(t *testing.T) {
	for _, raw := range []string{"", "depends_on ", "Depends_On", "DEPENDS_ON", "cites", "reuses", "dependency"} {
		typ := DependencyType(raw)
		if typ.Valid() {
			t.Errorf("Valid(%q) = true, want false: it is not one of the two catalog values", raw)
		}
		if typ.TriggersImpactAnalysis() {
			t.Errorf("TriggersImpactAnalysis(%q) = true, want false: a type the catalog does not know does not trigger re-analysis", raw)
		}
		if parsed, ok := ParseDependencyType(raw); ok || parsed != typ {
			t.Errorf("ParseDependencyType(%q) = (%q, %v), want (%q, false)", raw, parsed, ok, typ)
		}
	}
}

// TestParseDependencyTypeReadsBothValues: the two admissible values parse,
// and the parsed value IS the constant (a parse that returned a copy would
// make the two disagree the moment one changed).
func TestParseDependencyTypeReadsBothValues(t *testing.T) {
	for _, want := range AllDependencyTypes() {
		got, ok := ParseDependencyType(string(want))
		if !ok {
			t.Errorf("ParseDependencyType(%q) refused a value of the vocabulary", want)
			continue
		}
		if got != want {
			t.Errorf("ParseDependencyType(%q) = %q, want the constant itself", want, got)
		}
	}
}
