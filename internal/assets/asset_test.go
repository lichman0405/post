package assets

import (
	"strings"
	"testing"
)

// Task T0701 required test "asset core": the closed type set, the
// version label shape, and the visibility axis — the value domain every
// asset identity row must obey. The storage-side halves (the CHECK
// constraints and the pid column shape) are pinned by
// tests/integration/asset_core_test.go against a real PostgreSQL.

func TestAllTypesIsTheV1Set(t *testing.T) {
	got := AllTypes()
	want := []Type{TypeDataset, TypeProtocol, TypeMaterialCollection, TypeBenchmark}
	if len(got) != len(want) {
		t.Fatalf("AllTypes() = %v, want exactly the 4 V1 types %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AllTypes()[%d] = %q, want %q (order is part of the contract)", i, got[i], want[i])
		}
	}
}

func TestTypeValid(t *testing.T) {
	for _, ty := range AllTypes() {
		if !ty.Valid() {
			t.Errorf("type %q must be valid", ty)
		}
	}
	rejects := []string{"", "Dataset", "DATASET", "data_set", "datasets", "model", "paper", "dataset ", " dataset", "code"}
	for _, s := range rejects {
		if Type(s).Valid() {
			t.Errorf("type %q must be rejected: the V1 set is closed", s)
		}
	}
}

func TestParseType(t *testing.T) {
	cases := []struct {
		in   string
		want Type
		ok   bool
	}{
		{"dataset", TypeDataset, true},
		{" protocol ", TypeProtocol, true}, // surrounding whitespace is trimmed
		{"material_collection", TypeMaterialCollection, true},
		{"benchmark", TypeBenchmark, true},
		{"Dataset", "", false},
		{"", "", false},
		{"unknown", "", false},
	}
	for _, tc := range cases {
		got, ok := ParseType(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseType(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestVisibilityValid(t *testing.T) {
	if !VisibilityPublic.Valid() || !VisibilityPrivate.Valid() {
		t.Fatal("public and private must be the valid visibility values")
	}
	for _, v := range []Visibility{"", "internal", "unlisted", "Public", " project"} {
		if v.Valid() {
			t.Errorf("visibility %q must be rejected", v)
		}
	}
}

func TestValidVersionLabel(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"1", true},
		{"v1", true},
		{"1.0", true},
		{"2026-09-15", true},
		{"v2.0.1-rc.1", true},
		{"A", true},
		{"1_0", true},
		{" v1 ", true}, // trimmed before validation (domain convention)
		{"...", true},  // a dot-segment is only the whole-segment forms
		{"a..b", true},
		{".1", true},
	}
	for _, tc := range cases {
		if got := ValidVersionLabel(tc.in); got != tc.want {
			t.Errorf("ValidVersionLabel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	// The bound: exactly 64 characters is storable.
	exact := strings.Repeat("a", MaxVersionLen)
	if !ValidVersionLabel(exact) {
		t.Errorf("ValidVersionLabel(%d chars) = false, want true", MaxVersionLen)
	}

	// ValidVersionLabel has four rules — non-empty, an upper bound of
	// MaxVersionLen, the dot-segment exclusion, and the character set —
	// and each group below can only be refused by the rule it names.
	// Membership is asserted, not assumed: an off-bound case must be
	// entirely charset-legal (or the character set would be doing the
	// refusing) and a bad-character case must sit inside the bound (or the
	// length check would). The first T0701 delivery's over-bound case was
	// "v" plus 64 NUL bytes — 65 characters of which 64 are illegal, so
	// deleting the bound left the suite green.
	offBound := []struct{ s, why string }{
		{"", "empty"},
		{"   ", "whitespace only: empty after trimming"},
		{strings.Repeat("a", MaxVersionLen+1), "65 chars: one over the bound"},
	}
	for _, c := range offBound {
		trimmed := strings.TrimSpace(c.s)
		if len(trimmed) > 0 && len(trimmed) <= MaxVersionLen {
			t.Errorf("offBound case %q (%s) is inside 1..%d: only the bound may refuse it", c.s, c.why, MaxVersionLen)
		}
		for _, r := range trimmed {
			if !strings.ContainsRune(versionLabelCharset, r) {
				t.Errorf("offBound case %q (%s) contains %q outside the character set: only the bound may refuse it", c.s, c.why, r)
			}
		}
		if ValidVersionLabel(c.s) {
			t.Errorf("ValidVersionLabel(%q) = true, want false (%s)", c.s, c.why)
		}
	}
	dotSegments := []struct{ s, why string }{
		{".", "a dot segment, not a name"},
		{"..", "a dot segment, not a name"},
		{" .. ", "dot segment after trimming"},
	}
	for _, c := range dotSegments {
		trimmed := strings.TrimSpace(c.s)
		if trimmed != "." && trimmed != ".." {
			t.Errorf("dotSegments case %q (%s) is not a whole-segment dot form: only the dot-segment rule may refuse it", c.s, c.why)
		}
		if ValidVersionLabel(c.s) {
			t.Errorf("ValidVersionLabel(%q) = true, want false (%s)", c.s, c.why)
		}
	}
	badChar := []struct{ s, why string }{
		{"v 1", "space"},
		{"v/1", "slash"},
		{"v1/../..", "slash"},
		{"v1%20", "percent"},
		{"v1?x=1", "query"},
		{"#1", "fragment"},
	}
	for _, c := range badChar {
		trimmed := strings.TrimSpace(c.s)
		if len(trimmed) == 0 || len(trimmed) > MaxVersionLen {
			t.Errorf("badChar case %q (%s) is outside 1..%d: only the character set may refuse it", c.s, c.why, MaxVersionLen)
		}
		legal := true
		for _, r := range trimmed {
			if !strings.ContainsRune(versionLabelCharset, r) {
				legal = false
			}
		}
		if legal {
			t.Errorf("badChar case %q (%s) is entirely charset-legal: the length rule, not the character set, would refuse it", c.s, c.why)
		}
		if ValidVersionLabel(c.s) {
			t.Errorf("ValidVersionLabel(%q) = true, want false (%s)", c.s, c.why)
		}
	}
}
