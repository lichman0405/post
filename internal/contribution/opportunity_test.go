package contribution

import (
	"strings"
	"testing"
)

func TestOpportunityTargetTypeVocabulary(t *testing.T) {
	for _, ok := range []OpportunityTargetType{TargetIssue, TargetResearchQuestion} {
		if !ValidOpportunityTargetType(ok) {
			t.Errorf("ValidOpportunityTargetType(%q) = false, want true", ok)
		}
	}
	for _, bad := range []OpportunityTargetType{"", "paper", "ISSUE", "dataset"} {
		if ValidOpportunityTargetType(bad) {
			t.Errorf("ValidOpportunityTargetType(%q) = true, want false", bad)
		}
	}
}

func TestDifficultyVocabulary(t *testing.T) {
	for _, ok := range []Difficulty{DifficultyBeginner, DifficultyIntermediate, DifficultyAdvanced} {
		if !ValidDifficulty(ok) {
			t.Errorf("ValidDifficulty(%q) = false, want true", ok)
		}
	}
	for _, bad := range []Difficulty{"", "easy", "Beginner", "expert"} {
		if ValidDifficulty(bad) {
			t.Errorf("ValidDifficulty(%q) = true, want false", bad)
		}
	}
}

func TestOpportunityStateVocabularyAndMachine(t *testing.T) {
	for _, ok := range []OpportunityState{OpportunityStateSuggested, OpportunityStateOpen, OpportunityStateClosed} {
		if !ValidOpportunityState(ok) {
			t.Errorf("ValidOpportunityState(%q) = false, want true", ok)
		}
	}
	for _, bad := range []OpportunityState{"", "draft", "OPEN"} {
		if ValidOpportunityState(bad) {
			t.Errorf("ValidOpportunityState(%q) = true, want false", bad)
		}
	}

	// The suggested -> open -> closed map, exactly.
	allowed := map[[2]OpportunityState]bool{
		{OpportunityStateSuggested, OpportunityStateOpen}:   true,
		{OpportunityStateSuggested, OpportunityStateClosed}: true,
		{OpportunityStateOpen, OpportunityStateClosed}:      true,
	}
	all := []OpportunityState{OpportunityStateSuggested, OpportunityStateOpen, OpportunityStateClosed}
	for _, from := range all {
		for _, to := range all {
			want := allowed[[2]OpportunityState{from, to}]
			if got := CanTransitionOpportunityState(from, to); got != want {
				t.Errorf("CanTransitionOpportunityState(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}

	if !IsTerminalOpportunityState(OpportunityStateClosed) {
		t.Error("closed is terminal")
	}
	for _, s := range []OpportunityState{OpportunityStateSuggested, OpportunityStateOpen, ""} {
		if IsTerminalOpportunityState(s) {
			t.Errorf("IsTerminalOpportunityState(%q) = true, want false", s)
		}
	}
}

func TestOpportunityVisibilityVocabulary(t *testing.T) {
	for _, ok := range []OpportunityVisibility{OpportunityVisibilityInternal, OpportunityVisibilityPublic} {
		if !ValidOpportunityVisibility(ok) {
			t.Errorf("ValidOpportunityVisibility(%q) = false, want true", ok)
		}
	}
	for _, bad := range []OpportunityVisibility{"", "PUBLIC", "private", "unlisted"} {
		if ValidOpportunityVisibility(bad) {
			t.Errorf("ValidOpportunityVisibility(%q) = true, want false", bad)
		}
	}
}

func TestValidOpportunityTitle(t *testing.T) {
	for _, ok := range []string{"a", "Curate the dataset", strings.Repeat("x", 200)} {
		if !ValidOpportunityTitle(ok) {
			t.Errorf("ValidOpportunityTitle(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "   ", strings.Repeat("x", 201)} {
		if ValidOpportunityTitle(bad) {
			t.Errorf("ValidOpportunityTitle(%q) = true, want false", bad)
		}
	}
}

func TestValidOpportunityDescription(t *testing.T) {
	for _, ok := range []string{"", "context", strings.Repeat("x", 50_000)} {
		if !ValidOpportunityDescription(ok) {
			t.Errorf("ValidOpportunityDescription(len %d) = false, want true", len(ok))
		}
	}
	if ValidOpportunityDescription(strings.Repeat("x", 50_001)) {
		t.Error("ValidOpportunityDescription(50001) = true, want false")
	}
}

func TestValidRequiredCapabilities(t *testing.T) {
	for _, ok := range [][]string{nil, {}, {"python"}, {"data-curation", "python"}, {"a0-b0-c0", "x"}} {
		if !ValidRequiredCapabilities(ok) {
			t.Errorf("ValidRequiredCapabilities(%v) = false, want true", ok)
		}
	}
	long := strings.Repeat("x", 65) // the tag cap is 64 (1 + {0,63})
	eleven := make([]string, 11)
	for i := range eleven {
		eleven[i] = "tag" + strings.Repeat("0", i) // distinct valid tags
	}
	cases := [][]string{
		{""},           // blank
		{"UPPER"},      // uppercase
		{"-leading"},   // leading hyphen
		{"two words"},  // space
		{long},         // oversized tag
		{"dup", "dup"}, // duplicate
		eleven,         // over MaxRequiredCapabilities
	}
	for _, bad := range cases {
		if ValidRequiredCapabilities(bad) {
			t.Errorf("ValidRequiredCapabilities(%v) = true, want false", bad)
		}
	}
}

func TestSortedCapabilities(t *testing.T) {
	in := []string{"b", "a"}
	out := SortedCapabilities(in)
	if out[0] != "a" || out[1] != "b" {
		t.Errorf("SortedCapabilities(%v) = %v, want sorted", in, out)
	}
	if in[0] != "b" {
		t.Errorf("SortedCapabilities mutated its input: %v", in)
	}
	// The canonical form of an empty list is an empty array, never nil:
	// the store inserts the value verbatim, and nil would write NULL
	// where the column requires '{}'.
	if got := SortedCapabilities(nil); got == nil || len(got) != 0 {
		t.Errorf("SortedCapabilities(nil) = %#v, want a non-nil empty slice", got)
	}
}
