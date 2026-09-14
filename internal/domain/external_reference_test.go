package domain

import (
	"errors"
	"reflect"
	"testing"
)

func TestCanonicalExternalReferenceSourceTypes(t *testing.T) {
	got := CanonicalExternalReferenceSourceTypes()
	want := []string{"publication", "patent", "database", "standard", "vendor", "web", "other"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CanonicalExternalReferenceSourceTypes() = %v, want %v", got, want)
	}
	// Every canonical type must validate, and the valid set must be exactly
	// the canonical set (the schema enum's seven values, no more).
	for _, s := range got {
		if !ValidExternalReferenceSourceType(s) {
			t.Errorf("ValidExternalReferenceSourceType(%q) = false, want true", s)
		}
	}
	for _, bogus := range []string{"", "paper", "PUBLICATION", "doi", "article"} {
		if ValidExternalReferenceSourceType(bogus) {
			t.Errorf("ValidExternalReferenceSourceType(%q) = true, want false", bogus)
		}
	}
}

func TestNormalizeExternalIdentifier(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // "" means an error is expected
	}{
		// Bare DOI names are case-folded (DOI names are case-insensitive,
		// ISO 26324) but otherwise kept.
		{"bare doi lowercase", "10.1000/xyz", "10.1000/xyz"},
		{"bare doi uppercase folded", "10.1000/XYZ", "10.1000/xyz"},
		{"doi scheme", "doi:10.1000/xyz", "10.1000/xyz"},
		{"doi scheme uppercase prefix", "DOI:10.1000/xyz", "10.1000/xyz"},
		{"doi scheme with space", "doi: 10.1000/xyz", "10.1000/xyz"},
		{"doi.org url", "https://doi.org/10.1000/xyz", "10.1000/xyz"},
		{"doi.org url uppercase host", "https://DOI.ORG/10.1000/XYZ", "10.1000/xyz"},
		{"dx.doi.org url", "http://dx.doi.org/10.1000/xyz", "10.1000/xyz"},
		{"url mixed case folded", "https://doi.org/10.1000/AbC", "10.1000/abc"},
		{"whitespace trimmed", "  10.1000/xyz  ", "10.1000/xyz"},
		{"long registrant", "10.123456789/suffix", "10.123456789/suffix"},
		// The trim/match set is the explicit ASCII set (ASCIIWhitespace):
		// every member trims, and an internal VT keeps an identifier from
		// being DOI-shaped on both sides of the drift pin.
		{"tab trimmed doi", "\t10.1000/ABC\t", "10.1000/abc"},
		{"vertical tab trimmed", "\v10.1000/xyz\v", "10.1000/xyz"},
		{"form feed trimmed", "\f10.1000/xyz\f", "10.1000/xyz"},
		{"internal vertical tab passes through", "10.1000/ABC\vXYZ", "10.1000/ABC\vXYZ"},
		// Non-DOI identifiers are only whitespace-trimmed, never rewritten —
		// normalization is type-independent and must not mangle other
		// identifier vocabularies.
		{"patent number kept", "US1234567B2", "US1234567B2"},
		{"arxiv id kept", "arXiv:2401.00001", "arXiv:2401.00001"},
		{"non-doi url kept", "https://example.com/paper", "https://example.com/paper"},
		{"non-doi trimmed", "  ABC-123  ", "ABC-123"},
		// Empty or whitespace-only identifiers have no canonical form.
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeExternalIdentifier(tc.in)
			if tc.want == "" {
				if !errors.Is(err, ErrIdentifierEmpty) {
					t.Fatalf("NormalizeExternalIdentifier(%q) error = %v, want ErrIdentifierEmpty", tc.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeExternalIdentifier(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeExternalIdentifier(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
