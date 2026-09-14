package domain

import (
	"strings"
	"testing"
)

// TestValidProfileNameBounds: the profile name token is 1-64 characters of
// lowercase snake_case. The 64-character bound matters at the domain layer:
// the derived schema id must fit the database CHECK (char_length(schema_id)
// <= 512) with margin, and — the point — an over-long name is refused
// BEFORE the registry or the database ever see it, so it can never leave a
// registry entry whose row died on the CHECK constraint.
func TestValidProfileNameBounds(t *testing.T) {
	valid := []string{
		"catalysis",
		"a",
		"catalyst_screen_v2",
		"x1_2",
		strings.Repeat("a", 64),
	}
	for _, name := range valid {
		if !ValidProfileName(name) {
			t.Errorf("ValidProfileName(%q) = false, want true", name)
		}
	}
	invalid := []string{
		"",
		strings.Repeat("a", 65), // one past the bound
		strings.Repeat("a", 512),
		"Bad Name",
		"with-dash",
		"with.dot",
		"1leading_digit",
		"_leading_underscore",
		"café",
	}
	for _, name := range invalid {
		if ValidProfileName(name) {
			t.Errorf("ValidProfileName(%q) = true, want false", name)
		}
	}
}

// TestValidProfileVersionBounds: the version label mirrors the database
// CHECK's shape (1-64 characters of A-Za-z0-9._-).
func TestValidProfileVersionBounds(t *testing.T) {
	valid := []string{"1", "2", "v1.0", "2026-09-14", strings.Repeat("v", 64)}
	for _, v := range valid {
		if !ValidProfileVersion(v) {
			t.Errorf("ValidProfileVersion(%q) = false, want true", v)
		}
	}
	invalid := []string{"", "v/1", "v 1", strings.Repeat("v", 65), "a:b"}
	for _, v := range invalid {
		if ValidProfileVersion(v) {
			t.Errorf("ValidProfileVersion(%q) = true, want false", v)
		}
	}
}
