package domain

import (
	"encoding/json"
	"testing"
)

func TestValidClaimType(t *testing.T) {
	valid := []string{"descriptive", "quantitative", "comparative", "associational", "predictive", "causal", "mechanistic"}
	for _, s := range valid {
		if !ValidClaimType(s) {
			t.Errorf("ValidClaimType(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "Causal", "correlational", "CLAIM", "causal_claim"} {
		if ValidClaimType(s) {
			t.Errorf("ValidClaimType(%q) = true, want false", s)
		}
	}
}

func TestCausalClaimType(t *testing.T) {
	for _, tt := range []struct {
		typ  ClaimType
		want bool
	}{
		{ClaimTypeCausal, true},
		{ClaimTypeMechanistic, true},
		{ClaimTypeAssociational, false},
		{ClaimTypeDescriptive, false},
		{ClaimTypePredictive, false},
	} {
		if got := CausalClaimType(tt.typ); got != tt.want {
			t.Errorf("CausalClaimType(%q) = %v, want %v", tt.typ, got, tt.want)
		}
	}
}

func TestValidClaimAssessment(t *testing.T) {
	for _, s := range []string{"preliminary", "supported", "contested", "unresolved", "superseded", "aborted"} {
		if !ValidClaimAssessment(s) {
			t.Errorf("ValidClaimAssessment(%q) = false, want true", s)
		}
	}
	// The finding schema's "accepted" is deliberately NOT a claim
	// assessment (docs/43): the two enums must not blur.
	if ValidClaimAssessment("accepted") {
		t.Errorf("ValidClaimAssessment(%q) = true, want false", "accepted")
	}
}

func TestValidClaimBasisType(t *testing.T) {
	for _, s := range []string{"controlled_intervention", "temporal_ordering", "confounders_considered", "dose_response", "mechanistic_characterization", "computational_intervention"} {
		if !ValidClaimBasisType(s) {
			t.Errorf("ValidClaimBasisType(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "controlled intervention", "RCT", "causality"} {
		if ValidClaimBasisType(s) {
			t.Errorf("ValidClaimBasisType(%q) = true, want false", s)
		}
	}
}

func TestParseClaimScopeStructured(t *testing.T) {
	raw := json.RawMessage(`{"population":"bulk polycrystalline MOF-5",
		"conditions":[{"name":"temperature","value":298,"unit":"K"}],
		"statistics":{"n":3,"uncertainty":{"kind":"std","value":0.4,"unit":"cm3/g"}},
		"note":"free-form qualifiers alongside the structured ones"}`)
	s, ok := ParseClaimScope(raw)
	if !ok {
		t.Fatalf("ParseClaimScope(%s) = !ok, want ok", raw)
	}
	if s.Population != "bulk polycrystalline MOF-5" {
		t.Errorf("population = %q", s.Population)
	}
	if len(s.Conditions) != 1 || s.Conditions[0].Name != "temperature" || s.Conditions[0].Value != 298 || s.Conditions[0].Unit != "K" {
		t.Errorf("conditions = %+v", s.Conditions)
	}
	if s.Statistics == nil || s.Statistics.N != 3 || s.Statistics.Uncertainty == nil || s.Statistics.Uncertainty.Value != 0.4 {
		t.Errorf("statistics = %+v", s.Statistics)
	}
	if !s.Structured() {
		t.Error("Structured() = false for a scope with conditions and statistics")
	}
}

func TestParseClaimScopeRejectsUnstructured(t *testing.T) {
	cases := []struct {
		name string
		raw  json.RawMessage
	}{
		{"empty", nil},
		{"null scope", json.RawMessage(`null`)},
		{"free-text string", json.RawMessage(`"298 K"`)},
		{"array scope", json.RawMessage(`[{"name":"temperature"}]`)},
		{"malformed json", json.RawMessage(`{"population":`)},
		{"non-numeric condition value", json.RawMessage(`{"conditions":[{"name":"temperature","value":"RT"}]}`)},
		{"nameless condition", json.RawMessage(`{"conditions":[{"value":298,"unit":"K"}]}`)},
		{"non-array conditions", json.RawMessage(`{"conditions":{"name":"temperature"}}`)},
	}
	for _, tc := range cases {
		if _, ok := ParseClaimScope(tc.raw); ok {
			t.Errorf("%s: ParseClaimScope(%s) = ok, want !ok", tc.name, tc.raw)
		}
	}
}

func TestClaimScopeStructuredPopulationAloneIsNotStructure(t *testing.T) {
	// A population string alone is free text: it must not count as a
	// structured quantitative scope (the structure exists so the claim
	// can be filtered and compared, not so it can carry prose).
	s := ClaimScope{Population: "as-synthesized powder"}
	if s.Structured() {
		t.Error("Structured() = true for a population-only scope, want false")
	}
}

func TestClaimValidateBasis(t *testing.T) {
	ok := Claim{Basis: []ClaimBasis{
		{Type: ClaimBasisControlledIntervention, Detail: "pressure swing between 1 and 10 bar"},
		{Type: ClaimBasisDoseResponse},
	}}
	if err := ok.ValidateBasis(); err != nil {
		t.Errorf("ValidateBasis() = %v, want nil", err)
	}
	empty := Claim{Basis: nil}
	if err := empty.ValidateBasis(); err != nil {
		t.Errorf("ValidateBasis() of no basis = %v, want nil (absence is the warning case, not a structural error)", err)
	}
	bad := Claim{Basis: []ClaimBasis{{Type: "rct"}}}
	if err := bad.ValidateBasis(); err == nil {
		t.Error("ValidateBasis() of a non-canonical basis type = nil, want error")
	}
	untyped := Claim{Basis: []ClaimBasis{{Detail: "no type"}}}
	if err := untyped.ValidateBasis(); err == nil {
		t.Error("ValidateBasis() of a typeless entry = nil, want error")
	}
}
