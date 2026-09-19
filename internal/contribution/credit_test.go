package contribution

import (
	"strings"
	"testing"
)

// TestCreditRolesAreTheTwoTheSchemaAdmits pins the credit vocabulary to
// exactly the two values migration 00103's CHECK admits. docs/13 §2 names
// three roles and leaves the list open with 等; the open end is an L3
// decision that has not been taken, so this vocabulary refuses everything
// outside the two — "method_designer" included, because refusing it is the
// fail-closed answer until the decision is made.
//
// The test is written so that WIDENING the vocabulary fails here: an
// implementation that added a role to the Go list without a migration would
// otherwise be caught only by a database error at the first write.
func TestCreditRolesAreTheTwoTheSchemaAdmits(t *testing.T) {
	want := []CreditRole{CreditRoleCreator, CreditRoleMajorContributor}
	got := CreditRoles()
	if len(got) != len(want) {
		t.Fatalf("CreditRoles() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("CreditRoles()[%d] = %q, want %q", i, got[i], want[i])
		}
		if !ValidCreditRole(got[i]) {
			t.Errorf("ValidCreditRole(%q) = false for a role CreditRoles returns", got[i])
		}
	}
	for _, r := range []CreditRole{"", "method_designer", "Creator", "major-contributor", "creator "} {
		if ValidCreditRole(r) {
			t.Errorf("ValidCreditRole(%q) = true, want false (the 等 of docs/13 §2 is undecided)", r)
		}
	}

	// The returned slice is a copy: a caller cannot reorder or extend the
	// set the database admits.
	mutated := CreditRoles()
	mutated[0] = "method_designer"
	if CreditRoles()[0] != CreditRoleCreator {
		t.Error("CreditRoles() handed out the package's own slice")
	}
}

// TestCreditTargetRefsRoundTripAndRefuseLooseValues pins the canonical
// "kind:value" shape against the database's CHECK: the kind must be one
// docs/13 §2 names, and the value must be non-empty and must not itself
// carry the separator — a value with a colon in it would parse as a
// different kind on the way back.
func TestCreditTargetRefsRoundTripAndRefuseLooseValues(t *testing.T) {
	for _, kind := range []CreditTargetKind{CreditTargetAsset, CreditTargetRelease, CreditTargetFinding} {
		ref, ok := NewCreditTargetRef(kind, "0123456789abcdefghjkmnpqrs")
		if !ok {
			t.Fatalf("NewCreditTargetRef(%q, value) refused", kind)
		}
		if ref != string(kind)+":0123456789abcdefghjkmnpqrs" {
			t.Fatalf("ref = %q", ref)
		}
		back, value, ok := ParseCreditTargetRef(ref)
		if !ok || back != kind || value != "0123456789abcdefghjkmnpqrs" {
			t.Fatalf("ParseCreditTargetRef(%q) = %q, %q, %t", ref, back, value, ok)
		}
	}
	refuses := []struct {
		kind  CreditTargetKind
		value string
	}{
		{"", "x"},
		{"project", "x"},
		{"object", "x"},
		{CreditTargetAsset, ""},
		{CreditTargetAsset, "   "},
		{CreditTargetAsset, "a:b"},
	}
	for _, c := range refuses {
		if _, ok := NewCreditTargetRef(c.kind, c.value); ok {
			t.Errorf("NewCreditTargetRef(%q, %q) accepted", c.kind, c.value)
		}
	}
	for _, ref := range []string{"", "asset", "asset:", ":x", "asset:a:b", "project:x", "asset:x "} {
		if _, _, ok := ParseCreditTargetRef(ref); ok {
			t.Errorf("ParseCreditTargetRef(%q) accepted", ref)
		}
	}
	// The two helpers agree, which is what lets a reader trust a ref it can
	// parse: everything NewCreditTargetRef emits, ParseCreditTargetRef
	// accepts with the same kind.
	for _, kind := range []CreditTargetKind{CreditTargetAsset, CreditTargetRelease, CreditTargetFinding} {
		ref, _ := NewCreditTargetRef(kind, "v")
		if back, _, ok := ParseCreditTargetRef(ref); !ok || back != kind {
			t.Errorf("the ref %q NewCreditTargetRef emitted does not parse back", ref)
		}
	}
}

// TestDisputeStatesAndTerminality pins the three states 00011's CHECK
// admits and the property the guard enforces: a dispute is born open, and
// closing it is terminal — there is no way back, and "open" is not a
// decision (docs/13 §3's dispute is decided once; a later claim is a new
// dispute).
func TestDisputeStatesAndTerminality(t *testing.T) {
	for _, s := range []DisputeState{DisputeStateOpen, DisputeStateResolved, DisputeStateRejected} {
		if !ValidDisputeState(s) {
			t.Errorf("ValidDisputeState(%q) = false", s)
		}
	}
	for _, s := range []DisputeState{"", "closed", "OPEN", "Resolved", "decided", "open "} {
		if ValidDisputeState(s) {
			t.Errorf("ValidDisputeState(%q) = true", s)
		}
	}
	if DisputeStateOpen.Terminal() {
		t.Error("open is terminal — a dispute could never be decided")
	}
	if !DisputeStateResolved.Terminal() || !DisputeStateRejected.Terminal() {
		t.Error("a decided dispute is not terminal — it could be decided twice")
	}
}

// TestLedgerRefKindValidityIsClosed pins the check the credit command
// applies to a dispute's evidence: a ref whose kind the projection does not
// produce is refused, so a claim cannot be attached to evidence that names
// something the ledger cannot hold.
func TestLedgerRefKindValidityIsClosed(t *testing.T) {
	for _, k := range []LedgerRefKind{
		RefObject, RefObjectVersion, RefState, RefPullRequest, RefMerge,
		RefRelease, RefAsset, RefAssetVersion, RefKnowledgePublication, RefCreditDispute,
	} {
		if !ValidLedgerRefKind(k) {
			t.Errorf("ValidLedgerRefKind(%q) = false for a kind the projection produces", k)
		}
	}
	for _, k := range []LedgerRefKind{"", "credit_title", "OBJECT", "credit_dispute ", "finding"} {
		if ValidLedgerRefKind(k) {
			t.Errorf("ValidLedgerRefKind(%q) = true, want false", k)
		}
	}
	// Every mapping's declared ref kinds are valid kinds: a mapping that
	// pointed at a kind this list does not carry would produce a ref no
	// reader could classify.
	for _, m := range LedgerMappings() {
		for _, spec := range m.Refs {
			if !ValidLedgerRefKind(spec.Kind) {
				t.Errorf("mapping %q declares ref kind %q, which ValidLedgerRefKind refuses", m.EventType, spec.Kind)
			}
		}
	}
	// The dispute pair carries the dispute ref and it is the payload key
	// the producing store writes (persistence/credit_store.go).
	for _, eventType := range []string{"credit.dispute_opened", "credit.dispute_resolved"} {
		m, ok := LedgerMappingFor(eventType)
		if !ok {
			t.Fatalf("the ledger has no mapping for %s", eventType)
		}
		if len(m.Refs) != 1 || m.Refs[0].Kind != RefCreditDispute || m.Refs[0].PayloadKey != "dispute_id" {
			t.Fatalf("%s refs = %+v, want one credit_dispute ref read from dispute_id", eventType, m.Refs)
		}
		if strings.Contains(m.Why, "No producer exists yet") {
			t.Errorf("%s still says no producer exists: %s", eventType, m.Why)
		}
		if m.Why == "" {
			t.Errorf("%s has no rationale", eventType)
		}
	}
}
