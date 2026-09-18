package assets

import (
	"testing"
)

// Task T0707: the recording rule for asset_dependencies — "this project uses
// that fixed asset version". The table had a reader (the asset page's
// used/derived public links) and no writer at all; PublishedUsages is the
// rule the writer follows, and it is a pure function so that the rule can be
// tested without a database.
//
// The two things worth pinning here are the ones a reviewer would otherwise
// have to trust: that every recorded usage is a DEPENDENCY and not a
// citation (a manifest has no field that could declare a citation), and that
// the row's visibility_of_usage is the published version's OWN visibility
// rather than a default.

// usagePins builds the fixture's pins, asserting each one is well formed: a
// test about the recording rule must not pass because its input was not a
// pin at all.
func usagePins(t *testing.T, raw ...string) []DependencyPin {
	t.Helper()
	out := make([]DependencyPin, 0, len(raw))
	for _, r := range raw {
		pin := DependencyPin(r)
		if !pin.Valid() {
			t.Fatalf("the fixture names %q, which is not a pin", r)
		}
		out = append(out, pin)
	}
	return out
}

// TestPublishedUsagesRecordsEveryPinAsADependsOnUsage: one declaration per
// pin, in the manifest's order, each one a depends_on (docs/11 §5 puts "the
// exact version this work was built against" under Dependency, and the
// catalog gives exactly that value the DependencyInference flag).
func TestPublishedUsagesRecordsEveryPinAsADependsOnUsage(t *testing.T) {
	pins := usagePins(t, string(samplePID)+"@1.0", string(anotherPID)+"@2.3")
	got := PublishedUsages(pins, VisibilityPublic)
	if len(got) != len(pins) {
		t.Fatalf("PublishedUsages returned %d declarations for %d pins: %+v", len(got), len(pins), got)
	}
	for i, want := range pins {
		if got[i].Pin != want {
			t.Errorf("declaration %d names %q, want %q (order must be the manifest's)", i, got[i].Pin, want)
		}
		if got[i].Type != DependencyTypeDependsOn {
			t.Errorf("declaration %d is %q, want %q: a pinned version is what the work was built against", i, got[i].Type, DependencyTypeDependsOn)
		}
		if !got[i].Type.Valid() {
			t.Errorf("declaration %d carries a type outside the vocabulary: %q", i, got[i].Type)
		}
		if !got[i].Type.TriggersImpactAnalysis() {
			t.Errorf("declaration %d carries a type that does not trigger impact analysis: %q", i, got[i].Type)
		}
		if got[i].Type == DependencyTypeReferences {
			t.Errorf("declaration %d is recorded as a citation; a manifest cannot declare one (see usage.go)", i)
		}
	}
}

// TestPublishedUsagesTakesTheVersionsOwnVisibility: the declaration's
// visibility_of_usage is the published version's, verbatim. Both values are
// exercised, because the failing direction is the quiet one: a rule that
// defaulted to public would look right on every public publish and would
// record a private declaration as public.
func TestPublishedUsagesTakesTheVersionsOwnVisibility(t *testing.T) {
	pins := usagePins(t, string(samplePID)+"@1.0")
	for _, visibility := range []Visibility{VisibilityPublic, VisibilityPrivate} {
		got := PublishedUsages(pins, visibility)
		if len(got) != 1 {
			t.Fatalf("PublishedUsages(pins, %q) returned %d declarations, want 1", visibility, len(got))
		}
		if got[0].VisibilityOfUsage != visibility {
			t.Errorf("a version published %q declares its usages %q", visibility, got[0].VisibilityOfUsage)
		}
	}
}

// TestPublishedUsagesOfAVersionThatDependsOnNothingIsEmptyAndNotNil: an
// empty list is a fact ("this publish declares no dependency"), and a nil
// one would make the caller distinguish it from an absence it cannot
// distinguish.
func TestPublishedUsagesOfAVersionThatDependsOnNothingIsEmptyAndNotNil(t *testing.T) {
	got := PublishedUsages(nil, VisibilityPublic)
	if got == nil {
		t.Fatal("PublishedUsages(nil, ...) returned nil, want an empty list")
	}
	if len(got) != 0 {
		t.Errorf("PublishedUsages(nil, ...) = %+v, want no declarations", got)
	}
}

// TestPublishedUsagesDeduplicatesByPin: the publish gate already refuses a
// manifest that lists one pin twice, so this is the second line — but it is
// the line that decides whether one version can write two rows for one use.
func TestPublishedUsagesDeduplicatesByPin(t *testing.T) {
	first := string(samplePID) + "@1.0"
	second := string(anotherPID) + "@2.3"
	pins := usagePins(t, first, second, first)
	got := PublishedUsages(pins, VisibilityPublic)
	if len(got) != 2 {
		t.Fatalf("PublishedUsages with a repeated pin returned %d declarations, want 2: %+v", len(got), got)
	}
	if got[0].Pin != DependencyPin(first) || got[1].Pin != DependencyPin(second) {
		t.Errorf("deduplication reordered the declarations: %+v", got)
	}
}
