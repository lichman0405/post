package domain

import "testing"

// TestValidResolutionKind pins the decision vocabulary: exactly the five
// kinds docs/09 §8 names are valid, and nothing else — no computed,
// averaged or agent-chosen kind can sneak in through a spelling that
// happens to pass.
func TestValidResolutionKind(t *testing.T) {
	valid := []ResolutionKind{
		ResolutionAcceptSource,
		ResolutionAcceptTarget,
		ResolutionKeepBoth,
		ResolutionValidationBranch,
		ResolutionUnresolved,
	}
	for _, k := range valid {
		if !ValidResolutionKind(k) {
			t.Errorf("ValidResolutionKind(%q) = false, want true", k)
		}
	}
	invalid := []ResolutionKind{
		"", "accept_a", "accept_b", "keep-both", "average", "auto",
		"accept_source ", "ACCEPT_SOURCE", "validation branch",
	}
	for _, k := range invalid {
		if ValidResolutionKind(k) {
			t.Errorf("ValidResolutionKind(%q) = true, want false", k)
		}
	}
}

// TestValidResolutionTargetKind pins the two conflict targets.
func TestValidResolutionTargetKind(t *testing.T) {
	if !ValidResolutionTargetKind(ConflictResolutionTargetObject) {
		t.Error("object must be a valid target kind")
	}
	if !ValidResolutionTargetKind(ConflictResolutionTargetRelation) {
		t.Error("relation must be a valid target kind")
	}
	for _, k := range []ConflictResolutionTargetKind{"", "version", "field"} {
		if ValidResolutionTargetKind(k) {
			t.Errorf("ValidResolutionTargetKind(%q) = true, want false", k)
		}
	}
}
