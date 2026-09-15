package rights

import (
	"strings"
	"unicode/utf8"
)

// MaxLicenseIDLen bounds a standard license id. The unit is characters
// (Unicode code points), counted with utf8.RuneCountInString, as it is for
// the agreement ref and the metadata visibility token; see the note on
// MaxAgreementRefLen in document.go.
const MaxLicenseIDLen = 256

// ValidLicenseID reports whether id is a well-formed standard license
// identifier of the shape the license ecosystem uses: an SPDX-style id or
// expression ("MIT", "Apache-2.0", "CC-BY-4.0", "GPL-3.0-only WITH
// Classpath-exception-2.0", "MIT OR Apache-2.0") — 1..256 characters of
// letters, digits, '.', '-', '+', '(' and ')', with single spaces only
// between tokens, at least one letter or digit, and no surrounding space.
//
// This is a SYNTAX rule and nothing more, and the distinction is the whole
// point of the function:
//
//   - It does not check the id against the SPDX license list. No copy of
//     that list is vendored in this repository, and inventing one — a
//     hand-typed subset in Go — would reject well-formed ids that exist and
//     bless the day it goes stale. "NOT-A-REAL-LICENSE-1.0" is accepted by
//     this function; TestValidLicenseIDSyntaxOnlyNotAList pins that.
//
//     The rule is an alphabet rule, so it also accepts what a person types
//     when they mean an id but write the license's NAME: "CC BY 4.0" is
//     spelled with the same characters an expression is ("CC", "BY", "4.0")
//     and passes. Telling that string from a real expression needs the
//     list, so this function does not pretend to; the surface that collects
//     the value should offer ids, and what it stores is what the publisher
//     declared.
//
//   - It does not decide that the named license grants anything, applies
//     to the asset, or is compatible with anything else. ADR-009: 不发明新
//     的法律许可证; docs/38 §2: the platform does not answer questions of
//     contract interpretation or validity.
//
// What it does refuse is the value that means nothing: the empty string,
// whitespace, and the separators a copy-paste or a template leaves behind.
type LicenseID string

// Valid reports whether id is a well-formed license identifier.
func (id LicenseID) Valid() bool {
	return ValidLicenseID(string(id))
}

// ValidLicenseID reports whether s is a well-formed standard license
// identifier (see LicenseID for what "well-formed" does and does not
// mean). The 1..256 bound is counted in characters, matching the sentence
// that states it.
func ValidLicenseID(s string) bool {
	if s == "" || utf8.RuneCountInString(s) > MaxLicenseIDLen || strings.TrimSpace(s) != s {
		return false
	}
	// A doubled space is a separator typo, not a token boundary; the
	// rule keeps the stored expression canonical enough that two equal
	// declarations compare equal as strings.
	if strings.Contains(s, "  ") {
		return false
	}
	hasName := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			hasName = true
		case r == '.', r == '-', r == '+', r == '(', r == ')', r == ' ':
			// Expression punctuation: no token of its own.
		default:
			return false
		}
	}
	return hasName
}
