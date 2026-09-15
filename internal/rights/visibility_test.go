package rights

import (
	"strings"
	"testing"
)

// TestDataAccessIsClosed pins the axis that HAS a vocabulary. It is the
// same two words blob_attachments.access_level admits (migration 00032),
// so a rights document and a blob row cannot disagree about what "open"
// means by using different words for it.
func TestDataAccessIsClosed(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want DataAccess
		ok   bool
	}{
		{"open", DataAccessOpen, true},
		{" restricted ", DataAccessRestricted, true},
		{"private", "", false},
		{"public", "", false},
		{"Open", "", false},
		{"", "", false},
	} {
		got, ok := ParseDataAccess(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseDataAccess(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestMetadataVisibilityIsAnOpenToken pins the asymmetry that makes the
// two axes separate: data_access is one of two words, metadata visibility
// is a token this package does not enumerate (specs/policies/
// rights-template.yaml names no set for it; docs/12 §2 says the underlying
// model is a policy id/scope, not a boolean).
//
// The consequence is deliberate and testable: a policy reference the
// platform has never heard of is storable and round-trips verbatim, so
// closing this set later cannot invalidate a stored document.
func TestMetadataVisibilityIsAnOpenToken(t *testing.T) {
	accepted := []string{
		"project_policy",
		"policy:0f8a1c2e-0000-4000-8000-000000000000",
		"PUBLIC", // docs/55's data-class spelling, should a writer use it
		"private",
	}
	for _, token := range accepted {
		got, ok := ParseMetadataVisibility(token)
		if !ok || string(got) != token {
			t.Errorf("ParseMetadataVisibility(%q) = (%q, %v), want the token verbatim", token, got, ok)
		}
	}

	rejected := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"spaces only", "   "},
		{"leading space", " project_policy"},
		{"trailing space", "project_policy "},
		{"inner whitespace", "project policy"},
		{"newline", "project_policy\n"},
		{"control character", "project\u0000policy"},
		{"full-width punctuation", "project＿policy"},
		{"over the bound", strings.Repeat("a", MaxMetadataVisibilityLen+1)},
	}
	for _, tc := range rejected {
		if _, ok := ParseMetadataVisibility(tc.token); ok {
			t.Errorf("%s: ParseMetadataVisibility(%q) accepted, want refused", tc.name, tc.token)
		}
	}
	// The bound is inclusive, so the over-bound case above fails on the
	// length rule rather than on the test's arithmetic.
	if _, ok := ParseMetadataVisibility(strings.Repeat("a", MaxMetadataVisibilityLen)); !ok {
		t.Errorf("a token of exactly %d characters was refused", MaxMetadataVisibilityLen)
	}

	// The default is the template's value, and it is a legal token — the
	// fail-closed default cannot itself be unstorable.
	if !ValidMetadataVisibility(string(MetadataProjectPolicy)) {
		t.Errorf("the default %q is not a valid token", MetadataProjectPolicy)
	}
}

// TestVisibilityValidateReportsBothAxes checks that a document whose two
// access axes are both broken reports both, each under its own path —
// the pair exists to be independent, so one broken axis must not mask the
// other.
func TestVisibilityValidateReportsBothAxes(t *testing.T) {
	err := Visibility{Metadata: "", DataAccess: "public"}.Validate()
	if err == nil {
		t.Fatal("Visibility.Validate() = nil, want both axes reported")
	}
	got := map[string]string{}
	for _, e := range flatten(err) {
		got[e.Field] = e.Code
	}
	if got["visibility.metadata"] != CodeInvalidMetadataVisibility {
		t.Errorf("metadata code = %q, want %q", got["visibility.metadata"], CodeInvalidMetadataVisibility)
	}
	if got["visibility.data_access"] != CodeInvalidDataAccess {
		t.Errorf("data_access code = %q, want %q", got["visibility.data_access"], CodeInvalidDataAccess)
	}

	// And each axis validates alone: a document may be metadata-public
	// with the bytes restricted — the case CLAUDE.md invariant 7
	// (Knowledge visibility != Blob accessibility) exists for.
	open := Visibility{Metadata: "PUBLIC", DataAccess: DataAccessRestricted}
	if err := open.Validate(); err != nil {
		t.Errorf("metadata public + bytes restricted = %v, want nil", err)
	}
}
