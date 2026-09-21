package assets

import (
	"strings"
	"testing"
)

// Task T0706 required test "asset metadata tests" — the domain half.
//
// What is pinned here is the SHAPE of a storable metadata field: the two
// display bounds, the description's bound and its empty-clears value, and
// the one list rule (count, non-blank, item length). Every case below is
// decided by a validator that has to be able to say NO, so each test walks
// the boundary in both directions — the largest accepted value and the
// first refused one — rather than asserting a happy path.
//
// The rule the file deliberately does NOT contain is asserted here too: no
// charset, no URL shape, no uniqueness and no ordering. Those are not
// oversights to be tightened later by a helpful refactor; they are the
// stated scope of the rule (see the metadata.go file doc), and a revision
// surface that started refusing a DOI or an internal path would be
// refusing a reference the publisher declared.

// ---------------------------------------------------------------------------
// Display fields: slug and title

func TestValidMetadataSlug(t *testing.T) {
	max := strings.Repeat("s", SlugMaxLen)
	cases := []struct {
		name string
		slug string
		want bool
	}{
		{"ordinary", "governance-lab", true},
		{"single character", "s", true},
		{"exactly the bound", max, true},
		{"padded but within the bound once trimmed", "  " + max + "  ", true},
		{"one over the bound", max + "s", false},
		{"one over the bound after trimming", " " + max + "s ", false},
		{"empty", "", false},
		{"blank", "   ", false},
		{"only a newline", "\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidMetadataSlug(tc.slug); got != tc.want {
				t.Errorf("ValidMetadataSlug(%d bytes) = %v, want %v", len(tc.slug), got, tc.want)
			}
		})
	}
}

// TestValidMetadataSlugAcceptsWhatItDoesNotConstrain is the scope of the
// rule stated as an assertion: a slug is display metadata that never enters
// a URL (Asset.Slug), so nothing about its CONTENT is refused. If one of
// these ever goes red, a path-segment or charset rule has been invented
// here — and a value the publish would have created has become
// unreachable through a revision.
func TestValidMetadataSlugAcceptsWhatItDoesNotConstrain(t *testing.T) {
	for _, slug := range []string{
		"Mixed Case And Spaces",
		"slash/es/allowed",
		"unicode-ελληνικά-中文",
		"dots.and..dots",
		"100%25",
	} {
		if !ValidMetadataSlug(slug) {
			t.Errorf("ValidMetadataSlug(%q) = false: the slug rule constrains length only", slug)
		}
	}
}

func TestValidMetadataTitle(t *testing.T) {
	max := strings.Repeat("t", TitleMaxLen)
	cases := []struct {
		name  string
		title string
		want  bool
	}{
		{"ordinary", "Governance Subject", true},
		{"exactly the bound", max, true},
		{"padded but within the bound once trimmed", "  " + max + "  ", true},
		{"one over the bound", max + "t", false},
		{"one over the bound after trimming", " " + max + "t ", false},
		{"empty", "", false},
		{"blank", " \t ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidMetadataTitle(tc.title); got != tc.want {
				t.Errorf("ValidMetadataTitle(%d bytes) = %v, want %v", len(tc.title), got, tc.want)
			}
		})
	}
}

// The other half of the display-bound rule — that the revision's bound and
// the publish's bound are ONE number, because both write one column — is
// pinned in internal/application/assetmetadata, the only package that may
// import both (assetpublish imports this package, so an assertion here
// could not name it).

// ---------------------------------------------------------------------------
// Description: prose, and the empty string that clears it

func TestValidMetadataDescription(t *testing.T) {
	max := strings.Repeat("d", MaxDescriptionLen)
	cases := []struct {
		name        string
		description string
		want        bool
	}{
		{"empty clears the field", "", true},
		{"ordinary", "A dataset about grain boundaries.", true},
		{"exactly the bound", max, true},
		{"one over the bound", max + "d", false},
		{"whitespace is content, not padding", strings.Repeat(" ", 10), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidMetadataDescription(tc.description); got != tc.want {
				t.Errorf("ValidMetadataDescription(%d bytes) = %v, want %v", len(tc.description), got, tc.want)
			}
		})
	}
}

// TestDescriptionIsNotTrimmed is the difference between this field and
// every other one: a leading space in prose is content, so the rule
// measures the value AS GIVEN. The assertion is made over the bound, where
// the two rules disagree: a value of exactly MaxDescriptionLen+1 is refused
// whether or not it is padded, and a value that is exactly at the bound
// once padded would be ACCEPTED by a trimming rule and is refused here.
func TestDescriptionIsNotTrimmed(t *testing.T) {
	padded := " " + strings.Repeat("d", MaxDescriptionLen)
	if ValidMetadataDescription(padded) {
		t.Fatal("ValidMetadataDescription accepted a value over the bound: it trims, but the write stores " +
			"what the caller declared, so trimming here would validate a shorter string than the one stored")
	}
	if !ValidMetadataDescription(strings.Repeat("d", MaxDescriptionLen)) {
		t.Fatal("ValidMetadataDescription refused a value exactly at the bound")
	}
}

// ---------------------------------------------------------------------------
// Lists: keywords, contact, documentation

func TestValidMetadataList(t *testing.T) {
	cases := []struct {
		name string
		list []string
		want bool
	}{
		{"nil is the empty list", nil, true},
		{"empty clears the field", []string{}, true},
		{"one entry", []string{"grain-boundary"}, true},
		{"exactly the item bound", []string{strings.Repeat("k", MaxKeywordLen)}, true},
		{"one over the item bound", []string{strings.Repeat("k", MaxKeywordLen+1)}, false},
		{"a padded item within the bound once trimmed", []string{"  " + strings.Repeat("k", MaxKeywordLen) + "  "}, true},
		{"a blank entry", []string{"ok", "   "}, false},
		{"an empty entry", []string{"ok", ""}, false},
		{"a tab-only entry", []string{"\t"}, false},
		{"an over-long entry among short ones", []string{"ok", strings.Repeat("k", MaxKeywordLen+1)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidMetadataList(tc.list, MaxKeywords, MaxKeywordLen); got != tc.want {
				t.Errorf("ValidMetadataList(%q, %d, %d) = %v, want %v", tc.list, MaxKeywords, MaxKeywordLen, got, tc.want)
			}
		})
	}
}

// TestValidMetadataListCountBound walks the COUNT bound in both directions,
// at each of the three list fields' own numbers — the three are different
// limits on the same rule, and one of them being off by one is exactly the
// kind of drift a single shared case would miss.
func TestValidMetadataListCountBound(t *testing.T) {
	for _, bound := range []struct {
		field string
		items int
		item  int
	}{
		{"keywords", MaxKeywords, MaxKeywordLen},
		{"contact", MaxContacts, MaxContactLen},
		{"documentation", MaxDocumentation, MaxDocumentationLen},
	} {
		t.Run(bound.field, func(t *testing.T) {
			atBound := make([]string, bound.items)
			for i := range atBound {
				atBound[i] = "entry"
			}
			if !ValidMetadataList(atBound, bound.items, bound.item) {
				t.Errorf("refused %d entries, the bound is %d", len(atBound), bound.items)
			}
			overBound := append(append([]string{}, atBound...), "entry")
			if ValidMetadataList(overBound, bound.items, bound.item) {
				t.Errorf("accepted %d entries, the bound is %d", len(overBound), bound.items)
			}
		})
	}
}

// TestValidMetadataListDoesNotConstrainContent is the list rule's scope as
// an assertion. A contact is whatever reference the publisher declared —
// an address, a DOI, an internal path, a handle — and this build stores it
// without dereferencing it, so no shape may be required here.
func TestValidMetadataListDoesNotConstrainContent(t *testing.T) {
	for _, item := range []string{
		"https://doi.org/10.1000/xyz123",
		"not-an-email-at-all",
		"mailto:someone@example.org",
		"  internal path with spaces  ",
		"中文条目",
		"<xml>&entities</xml>",
	} {
		if !ValidMetadataList([]string{item}, 1, MaxContactLen) {
			t.Errorf("ValidMetadataList refused %q: the rule constrains length and blankness only", item)
		}
	}
}

// TestValidMetadataListAllowsDuplicates pins the rule the file doc states
// is deliberately absent: duplicate keywords are meaningless but they are
// storable, and a uniqueness check here would be inventing a product rule.
// If this goes red, a de-duplication was added — which would silently
// rewrite a caller's declared list rather than refuse it.
func TestValidMetadataListAllowsDuplicates(t *testing.T) {
	if !ValidMetadataList([]string{"a", "a", "a"}, MaxKeywords, MaxKeywordLen) {
		t.Error("duplicate entries were refused: uniqueness is a product decision (CLAUDE.md §5), not a storage rule")
	}
}
