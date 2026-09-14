package domain

import "testing"

// The canonical question states and hypothesis assessments mirror the
// schema enums (research_question.schema.json question_state,
// hypothesis.schema.json assessment) 1:1; the integration drift test
// re-pins them against the schema registry, this unit test pins the
// Valid() behaviour and the canonical lists themselves.
func TestQuestionStateCanonical(t *testing.T) {
	want := []string{"open", "partially_answered", "unresolved", "superseded", "aborted"}
	got := CanonicalQuestionStates()
	if len(got) != len(want) {
		t.Fatalf("CanonicalQuestionStates() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("canonical question state [%d] = %s, want %s", i, got[i], want[i])
		}
		if !ValidQuestionState(got[i]) {
			t.Errorf("ValidQuestionState(%q) = false, want true", got[i])
		}
	}
	for _, bad := range []string{"", "closed", "in_progress", "OPEN", "partially answered", "open,partially_answered"} {
		if ValidQuestionState(bad) {
			t.Errorf("ValidQuestionState(%q) = true, want false", bad)
		}
	}
}

func TestHypothesisAssessmentCanonical(t *testing.T) {
	want := []string{"proposed", "under_test", "supported", "weakened", "contested", "aborted"}
	got := CanonicalHypothesisAssessments()
	if len(got) != len(want) {
		t.Fatalf("CanonicalHypothesisAssessments() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("canonical assessment [%d] = %s, want %s", i, got[i], want[i])
		}
		if !ValidHypothesisAssessment(got[i]) {
			t.Errorf("ValidHypothesisAssessment(%q) = false, want true", got[i])
		}
	}
	for _, bad := range []string{"", "open", "closed", "Supported", "under test", "proposed,supported"} {
		if ValidHypothesisAssessment(bad) {
			t.Errorf("ValidHypothesisAssessment(%q) = true, want false", bad)
		}
	}
}

// The decoupling the task pins down: question states and assessments are
// scientific content, not issue states. Issues carry
// open/in_progress/closed/aborted (issues.state CHECK); the two
// vocabularies share only the spellings "open" (questions) and "aborted"
// (questions and assessments) — a coincidence of English, not a bridge.
// Nothing translates between the domains, and the issue-only states are
// never valid scientific states.
func TestQuestionStatesAreNotIssueStates(t *testing.T) {
	issueStates := map[string]bool{"open": true, "in_progress": true, "closed": true, "aborted": true}
	overlap := 0
	for _, qs := range CanonicalQuestionStates() {
		if issueStates[qs] {
			if qs != "open" && qs != "aborted" {
				t.Errorf("question state %q collides with an issue state name", qs)
			}
			overlap++
		}
	}
	for _, as := range CanonicalHypothesisAssessments() {
		if issueStates[as] {
			if as != "aborted" {
				t.Errorf("hypothesis assessment %q collides with an issue state name", as)
			}
			overlap++
		}
	}
	if overlap != 3 { // "open" + "aborted" in question states, "aborted" in assessments
		t.Errorf("shared spellings with issue states = %d, want exactly 3 (question \"open\", question \"aborted\", assessment \"aborted\")", overlap)
	}
	// The issue-only states are never scientific states.
	if ValidQuestionState("closed") || ValidQuestionState("in_progress") {
		t.Error("ValidQuestionState must not accept issue states")
	}
	if ValidHypothesisAssessment("closed") || ValidHypothesisAssessment("open") || ValidHypothesisAssessment("in_progress") {
		t.Error("ValidHypothesisAssessment must not accept issue states")
	}
}
