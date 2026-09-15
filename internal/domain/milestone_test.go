package domain

import "testing"

func TestValidMilestoneKind(t *testing.T) {
	valid := []MilestoneKind{
		MilestoneCandidateSelected, MilestonePaperSubmitted,
		MilestonePatentFiled, MilestoneExternalValidation, MilestoneCustom,
	}
	for _, k := range valid {
		if !ValidMilestoneKind(k) {
			t.Errorf("ValidMilestoneKind(%q) = false, want true", k)
		}
	}
	for _, k := range []MilestoneKind{"", "completed", "done", "released", "paper", "CANDIDATE_SELECTED"} {
		if ValidMilestoneKind(k) {
			t.Errorf("ValidMilestoneKind(%q) = true, want false", k)
		}
	}
}

func TestMilestoneKindDisplayName(t *testing.T) {
	want := map[MilestoneKind]string{
		MilestoneCandidateSelected:  "Candidate selected",
		MilestonePaperSubmitted:     "Paper submitted",
		MilestonePatentFiled:        "Patent filed",
		MilestoneExternalValidation: "External validation",
		MilestoneCustom:             "Custom",
	}
	for k, name := range want {
		if got := k.DisplayName(); got != name {
			t.Errorf("DisplayName(%q) = %q, want %q", k, got, name)
		}
	}
}

func TestValidMilestoneLabel(t *testing.T) {
	// Custom requires a label — the label IS the marker's name.
	for _, label := range []string{"", "   "} {
		if ValidMilestoneLabel(label, MilestoneCustom) {
			t.Errorf("ValidMilestoneLabel(%q, custom) = true, want false", label)
		}
	}
	if !ValidMilestoneLabel("First external replicate", MilestoneCustom) {
		t.Error("ValidMilestoneLabel(custom with label) = false, want true")
	}
	// Canonical kinds may carry a blank or a present label.
	for _, k := range []MilestoneKind{
		MilestoneCandidateSelected, MilestonePaperSubmitted,
		MilestonePatentFiled, MilestoneExternalValidation,
	} {
		if !ValidMilestoneLabel("", k) || !ValidMilestoneLabel("  JACS 2026  ", k) {
			t.Errorf("ValidMilestoneLabel for kind %q rejected a legal label", k)
		}
	}
	// Every kind rejects an oversized label.
	for _, k := range []MilestoneKind{MilestoneCustom, MilestonePaperSubmitted} {
		if ValidMilestoneLabel("x"+repeat("a", MaxMilestoneLabelLen), k) {
			t.Errorf("ValidMilestoneLabel(oversized, %q) = true, want false", k)
		}
	}
	if !ValidMilestoneLabel("x"+repeat("a", MaxMilestoneLabelLen-1), MilestoneCustom) {
		t.Error("ValidMilestoneLabel(at bound) = false, want true")
	}
}

// TestNoCompletedTerminalState guards the acceptance "Project 没有强制
// Completed 终态" at the domain level: the project lifecycle vocabulary
// admits no "completed" state (docs/43 §1 — planning → active ↔ paused →
// archived, archived reactivatable), and no milestone kind is a
// completion state — milestones are timeline facts, never lifecycle
// transitions.
func TestNoCompletedTerminalState(t *testing.T) {
	if ValidActivityStatus("completed") {
		t.Error("ValidActivityStatus(completed) = true — the project lifecycle must keep no forced completed terminal state")
	}
	for _, k := range []MilestoneKind{
		MilestoneCandidateSelected, MilestonePaperSubmitted,
		MilestonePatentFiled, MilestoneExternalValidation, MilestoneCustom,
	} {
		if ValidActivityStatus(string(k)) {
			t.Errorf("milestone kind %q collides with a project activity status — milestones must stay timeline facts, never lifecycle states", k)
		}
	}
}
