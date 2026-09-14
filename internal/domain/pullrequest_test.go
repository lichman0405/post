package domain

import (
	"strings"
	"testing"
)

func TestValidPullRequestState(t *testing.T) {
	valid := []PullRequestState{
		PullRequestStateOpen, PullRequestStateReviewRequired,
		PullRequestStateChangesRequested, PullRequestStateApproved,
		PullRequestStateMergeReady, PullRequestStateMerged,
		PullRequestStateClosed, PullRequestStateAborted,
	}
	for _, s := range valid {
		if !ValidPullRequestState(s) {
			t.Errorf("ValidPullRequestState(%q) = false, want true", s)
		}
	}
	for _, s := range []PullRequestState{"", "draft", "OPEN", "in_review", "open "} {
		if ValidPullRequestState(s) {
			t.Errorf("ValidPullRequestState(%q) = true, want false", s)
		}
	}
}

func TestPullRequestStateTransitions(t *testing.T) {
	// Allowed edges (docs/43: open → review_required →
	// changes_requested/approved → merge_ready → merged; closed/aborted
	// from any non-terminal state; the review loop re-enters
	// review_required from changes_requested).
	allowed := []struct{ from, to PullRequestState }{
		{PullRequestStateOpen, PullRequestStateReviewRequired},
		{PullRequestStateOpen, PullRequestStateClosed},
		{PullRequestStateOpen, PullRequestStateAborted},
		{PullRequestStateReviewRequired, PullRequestStateChangesRequested},
		{PullRequestStateReviewRequired, PullRequestStateApproved},
		{PullRequestStateReviewRequired, PullRequestStateClosed},
		{PullRequestStateReviewRequired, PullRequestStateAborted},
		{PullRequestStateChangesRequested, PullRequestStateReviewRequired},
		{PullRequestStateChangesRequested, PullRequestStateClosed},
		{PullRequestStateChangesRequested, PullRequestStateAborted},
		{PullRequestStateApproved, PullRequestStateMergeReady},
		{PullRequestStateApproved, PullRequestStateClosed},
		{PullRequestStateApproved, PullRequestStateAborted},
		{PullRequestStateMergeReady, PullRequestStateMerged},
		{PullRequestStateMergeReady, PullRequestStateClosed},
		{PullRequestStateMergeReady, PullRequestStateAborted},
	}
	for _, e := range allowed {
		if !CanTransitionPullRequestState(e.from, e.to) {
			t.Errorf("CanTransitionPullRequestState(%q, %q) = false, want true", e.from, e.to)
		}
	}
	// Forbidden edges: skipping the machine, reopening anything, moving
	// terminal states, and open as a target.
	forbidden := []struct{ from, to PullRequestState }{
		{PullRequestStateOpen, PullRequestStateMerged},
		{PullRequestStateOpen, PullRequestStateMergeReady},
		{PullRequestStateOpen, PullRequestStateOpen},
		{PullRequestStateReviewRequired, PullRequestStateMerged},
		{PullRequestStateReviewRequired, PullRequestStateMergeReady},
		{PullRequestStateChangesRequested, PullRequestStateApproved},
		{PullRequestStateApproved, PullRequestStateMerged},
		{PullRequestStateMergeReady, PullRequestStateApproved},
		{PullRequestStateMerged, PullRequestStateOpen},
		{PullRequestStateMerged, PullRequestStateClosed},
		{PullRequestStateClosed, PullRequestStateOpen},
		{PullRequestStateAborted, PullRequestStateReviewRequired},
	}
	for _, e := range forbidden {
		if CanTransitionPullRequestState(e.from, e.to) {
			t.Errorf("CanTransitionPullRequestState(%q, %q) = true, want false", e.from, e.to)
		}
	}
}

func TestIsTerminalPullRequestState(t *testing.T) {
	for _, s := range []PullRequestState{
		PullRequestStateMerged, PullRequestStateClosed, PullRequestStateAborted,
	} {
		if !IsTerminalPullRequestState(s) {
			t.Errorf("IsTerminalPullRequestState(%q) = false, want true", s)
		}
	}
	for _, s := range []PullRequestState{
		PullRequestStateOpen, PullRequestStateReviewRequired,
		PullRequestStateChangesRequested, PullRequestStateApproved,
		PullRequestStateMergeReady,
	} {
		if IsTerminalPullRequestState(s) {
			t.Errorf("IsTerminalPullRequestState(%q) = true, want false", s)
		}
	}
}

func TestValidPullRequestTitle(t *testing.T) {
	valid := []string{"Add screening protocol", "  标题周围的空白可接受  "}
	for _, title := range valid {
		if !ValidPullRequestTitle(title) {
			t.Errorf("ValidPullRequestTitle(%q) = false, want true", title)
		}
	}
	invalid := []string{"", "   ", strings.Repeat("x", 201)}
	for _, title := range invalid {
		if ValidPullRequestTitle(title) {
			t.Errorf("ValidPullRequestTitle(%q) = true, want false", title)
		}
	}
}

func TestValidPullRequestBody(t *testing.T) {
	if !ValidPullRequestBody("") {
		t.Error("empty body must be valid (the column defaults to '')")
	}
	if !ValidPullRequestBody("some context") {
		t.Error("ordinary body rejected")
	}
	if ValidPullRequestBody(strings.Repeat("y", 50_001)) {
		t.Error("oversized body accepted")
	}
}
