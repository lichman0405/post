package domain

import (
	"strings"
	"testing"
)

func TestValidReviewKind(t *testing.T) {
	if !ValidReviewKind(ReviewKindScientific) || !ValidReviewKind(ReviewKindIntegrity) {
		t.Fatalf("scientific and integrity must be valid review kinds")
	}
	for _, k := range []ReviewKind{"", "rights", "ip", "approval", "Scientific"} {
		if ValidReviewKind(k) {
			t.Fatalf("ValidReviewKind(%q) = true, want false", k)
		}
	}
}

func TestValidReviewDecision(t *testing.T) {
	for _, d := range []ReviewDecision{ReviewDecisionApproved, ReviewDecisionChangesRequested, ReviewDecisionComment} {
		if !ValidReviewDecision(d) {
			t.Fatalf("ValidReviewDecision(%q) = false, want true", d)
		}
	}
	for _, d := range []ReviewDecision{"", "commented", "approve", "changes-requested"} {
		if ValidReviewDecision(d) {
			t.Fatalf("ValidReviewDecision(%q) = true, want false", d)
		}
	}
}

func TestValidReviewBody(t *testing.T) {
	if !ValidReviewBody("") || !ValidReviewBody("reasonable") {
		t.Fatal("empty and reasonable bodies must be valid")
	}
	if ValidReviewBody(strings.Repeat("x", 50_001)) {
		t.Fatal("oversized body must be invalid")
	}
}

func TestValidReviewResponsibility(t *testing.T) {
	for _, l := range []string{"", "Experimental Reviewer", strings.Repeat("r", 200)} {
		if !ValidReviewResponsibility(l) {
			t.Fatalf("ValidReviewResponsibility(%q) = false, want true", l)
		}
	}
	for _, l := range []string{"   ", strings.Repeat("r", 201)} {
		if ValidReviewResponsibility(l) {
			t.Fatalf("ValidReviewResponsibility(%q) = true, want false", l)
		}
	}
}
