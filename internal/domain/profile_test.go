package domain

import (
	"strings"
	"testing"
)

func TestValidBio(t *testing.T) {
	cases := []struct {
		name string
		bio  string
		want bool
	}{
		{"empty clears", "", true},
		{"short text", "computational materials scientist", true},
		{"multibyte text", "计算材料学 — 第一性原理", true},
		{"exactly at the bound", strings.Repeat("b", MaxBioLen), true},
		{"one over the bound", strings.Repeat("b", MaxBioLen+1), false},
	}
	for _, tc := range cases {
		if got := ValidBio(tc.bio); got != tc.want {
			t.Errorf("ValidBio(%d bytes) = %v, want %v", len(tc.bio), got, tc.want)
		}
	}
}

func TestValidDisplayName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"Alice", true},
		{"李 小", true},
		{"a", true},
		{"", false},
		{"   ", false},
		{"\t\n", false},
		{strings.Repeat("n", MaxDisplayNameLen), true},
		{strings.Repeat("n", MaxDisplayNameLen+1), false},
	}
	for _, tc := range cases {
		if got := ValidDisplayName(tc.name); got != tc.want {
			t.Errorf("ValidDisplayName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
