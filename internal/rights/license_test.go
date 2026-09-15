package rights

import (
	"strings"
	"testing"
)

// TestValidLicenseIDAcceptsTheEcosystemForms walks the ids a publisher
// actually writes. The accepted set is the license ecosystem's own
// spellings, including the two shapes a strict "one id" reading would have
// refused: an SPDX expression (MIT OR Apache-2.0) and a WITH exception
// (GPL-3.0-only WITH Classpath-exception-2.0).
func TestValidLicenseIDAcceptsTheEcosystemForms(t *testing.T) {
	accepted := []string{
		"MIT",
		"Apache-2.0",
		"CC-BY-4.0",
		"CC0-1.0",
		"GPL-3.0-only",
		"GPL-3.0-only WITH Classpath-exception-2.0",
		"MIT OR Apache-2.0",
		"(MIT OR Apache-2.0)",
		"CC-BY-NC-SA-4.0",
		"BSD-3-Clause",
		// ADR-009 does not license this platform to judge the list: a
		// well-formed id is storable whatever it names.
		"NOT-A-REAL-LICENSE-1.0",
	}
	for _, id := range accepted {
		if !ValidLicenseID(id) {
			t.Errorf("ValidLicenseID(%q) = false, want true", id)
		}
	}
}

// TestValidLicenseIDRejectsEmptyAndMalformed walks the other side. Each
// case is a value that means nothing where the field expects an id: the
// empty string (the template's "none named" is null, not ""), surrounding
// whitespace, the separators a copy-paste leaves behind, and characters
// no id is spelled with.
func TestValidLicenseIDRejectsEmptyAndMalformed(t *testing.T) {
	rejected := []struct {
		name string
		id   string
	}{
		{"empty", ""},
		{"only a space", " "},
		{"leading space", " MIT"},
		{"trailing space", "MIT "},
		{"double space", "MIT  OR  Apache-2.0"},
		{"newline", "MIT\n"},
		{"tab-separated", "MIT\tOR\tApache-2.0"},
		{"punctuation only", "()"},
		{"hyphen only", "-"},
		{"plus only", "+."},
		{"comma-separated", "MIT,Apache-2.0"},
		{"slash", "MIT/Apache-2.0"},
		{"non-ascii", "MIT—"},
		{"over the bound", strings.Repeat("A", MaxLicenseIDLen+1)},
	}
	for _, tc := range rejected {
		if ValidLicenseID(tc.id) {
			t.Errorf("%s: ValidLicenseID(%q) = true, want false", tc.name, tc.id)
		}
	}
	// The bound itself is inclusive: an id of exactly MaxLicenseIDLen
	// characters is accepted, so the case above fails on the length rule
	// and not on an off-by-one in the test's own expectation.
	if id := strings.Repeat("A", MaxLicenseIDLen); !ValidLicenseID(id) {
		t.Errorf("ValidLicenseID(len %d) = false, want true", MaxLicenseIDLen)
	}
}

// TestValidLicenseIDSyntaxOnlyNotAList is the honest half of the rule, and
// it is pinned as a positive assertion so that a future change adding a
// vendored SPDX list has to delete this test on purpose.
//
// The function is a syntax rule: it does not know the SPDX list (none is
// vendored in this repository), so a well-formed id that no registry
// contains is accepted, and — the direction that matters for a reader
// trusting a green light — ValidLicenseID("") is false while
// ValidLicenseID("NOT-A-REAL-LICENSE-1.0") is true. Neither answer says
// anything about what may be done with the data (docs/38 §2).
func TestValidLicenseIDSyntaxOnlyNotAList(t *testing.T) {
	if !ValidLicenseID("NOT-A-REAL-LICENSE-1.0") {
		t.Fatal("a well-formed unknown id was refused: this function would then be claiming list membership it does not have")
	}
	if ValidLicenseID("") {
		t.Fatal("the empty string was accepted as an id")
	}
	// The rule is an alphabet rule, and this is its known and accepted
	// weakness, pinned so that nothing downstream reads a green
	// ValidLicenseID as "this is a license": a license NAME is spelled
	// with the same characters an expression is.
	if !ValidLicenseID("CC BY 4.0") {
		t.Fatal("the alphabet rule now rejects a name-shaped value: it is claiming list knowledge it does not have")
	}
}

// TestLicenseIDValidMethodAgreesWithTheFunction keeps the method and the
// function from drifting: Document.Validate calls the function, a caller
// holding a LicenseID calls the method.
func TestLicenseIDValidMethodAgreesWithTheFunction(t *testing.T) {
	for _, id := range []string{"", "MIT", " MIT", "MIT OR Apache-2.0", "not a license"} {
		if got, want := LicenseID(id).Valid(), ValidLicenseID(id); got != want {
			t.Errorf("LicenseID(%q).Valid() = %v, ValidLicenseID = %v", id, got, want)
		}
	}
}
