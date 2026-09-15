package domain

import "testing"

func TestValidFindingType(t *testing.T) {
	for _, s := range []string{"observation", "comparison", "trend", "mechanistic_interpretation", "negative_finding", "integrated_conclusion"} {
		if !ValidFindingType(s) {
			t.Errorf("ValidFindingType(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "Negative", "negation", "positive", "summary", "conclusion"} {
		if ValidFindingType(s) {
			t.Errorf("ValidFindingType(%q) = true, want false", s)
		}
	}
}

func TestCanonicalFindingTypes(t *testing.T) {
	want := []string{"observation", "comparison", "trend", "mechanistic_interpretation", "negative_finding", "integrated_conclusion"}
	got := CanonicalFindingTypes()
	if len(got) != len(want) {
		t.Fatalf("CanonicalFindingTypes() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("canonical finding type [%d] = %s, want %s", i, got[i], want[i])
		}
		if !ValidFindingType(got[i]) {
			t.Errorf("ValidFindingType(%q) = false, want true", got[i])
		}
	}
}

func TestFindingTypeNegative(t *testing.T) {
	if !FindingTypeNegative.Negative() {
		t.Error("negative_finding.Negative() = false, want true")
	}
	for _, typ := range []FindingType{FindingTypeObservation, FindingTypeComparison, FindingTypeTrend, FindingTypeMechanisticInterpretation, FindingTypeIntegratedConclusion} {
		if typ.Negative() {
			t.Errorf("%s.Negative() = true, want false", typ)
		}
	}
}

func TestValidFindingAssessment(t *testing.T) {
	for _, s := range []string{"preliminary", "accepted", "contested", "unresolved", "superseded", "aborted"} {
		if !ValidFindingAssessment(s) {
			t.Errorf("ValidFindingAssessment(%q) = false, want true", s)
		}
	}
	// The claim schema's "supported" is deliberately NOT a finding
	// assessment (docs/43, finding.schema.json): a claim is supported by
	// evidence, a human-confirmed finding is accepted — the two enums
	// must not blur.
	if ValidFindingAssessment("supported") {
		t.Errorf("ValidFindingAssessment(%q) = true, want false", "supported")
	}
	for _, s := range []string{"", "Accepted", "verified", "rejected", "open", "closed"} {
		if ValidFindingAssessment(s) {
			t.Errorf("ValidFindingAssessment(%q) = true, want false", s)
		}
	}
}

func TestCanonicalFindingAssessments(t *testing.T) {
	want := []string{"preliminary", "accepted", "contested", "unresolved", "superseded", "aborted"}
	got := CanonicalFindingAssessments()
	if len(got) != len(want) {
		t.Fatalf("CanonicalFindingAssessments() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("canonical finding assessment [%d] = %s, want %s", i, got[i], want[i])
		}
		if !ValidFindingAssessment(got[i]) {
			t.Errorf("ValidFindingAssessment(%q) = false, want true", got[i])
		}
	}
}

func TestFindingValidateClaimVersionRefs(t *testing.T) {
	ok := Finding{ClaimVersionRefs: []string{"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"}}
	if err := ok.ValidateClaimVersionRefs(); err != nil {
		t.Errorf("ValidateClaimVersionRefs() = %v, want nil", err)
	}
	// Upper-case hex is the same canonical form.
	mixed := Finding{ClaimVersionRefs: []string{"AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"}}
	if err := mixed.ValidateClaimVersionRefs(); err != nil {
		t.Errorf("ValidateClaimVersionRefs() of upper-case uuid = %v, want nil", err)
	}

	cases := []struct {
		name string
		refs []string
	}{
		{"no refs", nil},
		{"empty ref", []string{""}},
		{"whitespace ref", []string{"  11111111-1111-4111-8111-111111111111 "}},
		{"not a uuid", []string{"claim-v1"}},
		{"uuid missing a group", []string{"11111111-1111-4111-8111-11111111111"}},
		{"uuid with a fifth group", []string{"11111111-1111-4111-8111-111111111111-9999"}},
		{"braced uuid", []string{"{11111111-1111-4111-8111-111111111111}"}},
	}
	for _, tc := range cases {
		if err := (Finding{ClaimVersionRefs: tc.refs}).ValidateClaimVersionRefs(); err == nil {
			t.Errorf("%s: ValidateClaimVersionRefs() = nil, want error", tc.name)
		}
	}
}

func TestValidUUID(t *testing.T) {
	for _, s := range []string{
		"11111111-1111-4111-8111-111111111111",
		"AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA",
	} {
		if !ValidUUID(s) {
			t.Errorf("ValidUUID(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "not-a-uuid", "11111111-1111-4111-8111", "11111111-1111-4111-8111-1111111111111", "zg111111-1111-4111-8111-111111111111"} {
		if ValidUUID(s) {
			t.Errorf("ValidUUID(%q) = true, want false", s)
		}
	}
}
