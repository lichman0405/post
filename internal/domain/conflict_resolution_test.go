package domain

import "testing"

// TestValidResolutionKind pins the decision vocabulary: exactly the
// actions docs/09 §8 names — all seven of them, plus the contested/
// unresolved state that section names for main — are valid, and nothing
// else, so no computed, averaged or agent-chosen kind can sneak in
// through a spelling that happens to pass. The list is the spec's own
// list: Accept A, Accept B, Keep both versions, Explicit coexistence,
// Create validation branch, Request more evidence, Abort proposed
// change. A test that only enumerated a subset would let the merge
// engine's vocabulary drift back below what the spec requires.
func TestValidResolutionKind(t *testing.T) {
	valid := []ResolutionKind{
		ResolutionAcceptSource,
		ResolutionAcceptTarget,
		ResolutionKeepBoth,
		ResolutionExplicitCoexistence,
		ResolutionValidationBranch,
		ResolutionRequestEvidence,
		ResolutionAbortChange,
		ResolutionUnresolved,
	}
	if len(valid) != 8 {
		t.Fatalf("the docs/09 §8 vocabulary has 7 actions plus contested/unresolved; this test enumerates %d", len(valid))
	}
	for _, k := range valid {
		if !ValidResolutionKind(k) {
			t.Errorf("ValidResolutionKind(%q) = false, want true", k)
		}
	}
	invalid := []ResolutionKind{
		"", "accept_a", "accept_b", "keep-both", "average", "auto",
		"accept_source ", "ACCEPT_SOURCE", "validation branch",
		"coexist", "explicit coexistence", "contested", "merge_both",
	}
	for _, k := range invalid {
		if ValidResolutionKind(k) {
			t.Errorf("ValidResolutionKind(%q) = true, want false", k)
		}
	}
}

// TestResolutionKindWireValues pins the stored spellings: these strings
// are the conflict_resolutions.resolution CHECK values (migration 00069)
// and the wire vocabulary the resolution UI sends — a rename here without
// the matching migration is a store failure, so the spellings are part of
// the contract.
func TestResolutionKindWireValues(t *testing.T) {
	want := map[ResolutionKind]string{
		ResolutionAcceptSource:        "accept_source",
		ResolutionAcceptTarget:        "accept_target",
		ResolutionKeepBoth:            "keep_both",
		ResolutionExplicitCoexistence: "explicit_coexistence",
		ResolutionValidationBranch:    "validation_branch",
		ResolutionRequestEvidence:     "request_evidence",
		ResolutionAbortChange:         "abort_change",
		ResolutionUnresolved:          "unresolved",
	}
	if len(want) != 8 {
		t.Fatalf("want 8 spellings, have %d", len(want))
	}
	for kind, spelling := range want {
		if string(kind) != spelling {
			t.Errorf("kind %q != stored spelling %q", string(kind), spelling)
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
