package semantics

import (
	"encoding/json"
	"testing"

	"github.com/lichman0405/post/internal/domain"
)

func hasHintCode(t *testing.T, hints []Hint, code string) bool {
	t.Helper()
	for _, h := range hints {
		if h.Code == code {
			return true
		}
	}
	return false
}

func TestCausalClaimWithoutBasisWarns(t *testing.T) {
	// The acceptance criterion in its own words: causal 无 basis 产生
	// validation warning. The warning is advisory — the write proceeds,
	// the hint tells the author what the declaration would buy.
	for _, typ := range []domain.ClaimType{domain.ClaimTypeCausal, domain.ClaimTypeMechanistic} {
		errs, hints := CheckClaimStructure(domain.Claim{
			Statement: "pressure swing increases CO2 capacity",
			Type:      typ,
		})
		if len(errs) != 0 {
			t.Errorf("%s claim without basis: errs = %v, want none", typ, errs)
		}
		if !hasHintCode(t, hints, HintCausalBasisMissing) {
			t.Errorf("%s claim without basis: hints = %+v, want %s", typ, hints, HintCausalBasisMissing)
		}
	}
}

func TestCausalClaimWithBasisDoesNotWarn(t *testing.T) {
	for _, typ := range []domain.ClaimType{domain.ClaimTypeCausal, domain.ClaimTypeMechanistic} {
		errs, hints := CheckClaimStructure(domain.Claim{
			Type:  typ,
			Basis: []domain.ClaimBasis{{Type: domain.ClaimBasisControlledIntervention, Detail: "pressure swing 1-10 bar"}},
		})
		if len(errs) != 0 {
			t.Errorf("%s claim with basis: errs = %v, want none", typ, errs)
		}
		if hasHintCode(t, hints, HintCausalBasisMissing) {
			t.Errorf("%s claim with basis: got %s, want no basis warning", typ, HintCausalBasisMissing)
		}
	}
}

func TestNonCausalClaimWithoutBasisDoesNotWarn(t *testing.T) {
	// Only causal/mechanistic claims owe a basis declaration (docs/10
	// §5); an associational claim must never be pushed toward causal
	// vocabulary (CLAUDE.md §9.12).
	for _, typ := range []domain.ClaimType{domain.ClaimTypeDescriptive, domain.ClaimTypeAssociational, domain.ClaimTypeQuantitative, domain.ClaimTypeComparative, domain.ClaimTypePredictive} {
		errs, hints := CheckClaimStructure(domain.Claim{Type: typ})
		if len(errs) != 0 {
			t.Errorf("%s claim: errs = %v, want none", typ, errs)
		}
		if hasHintCode(t, hints, HintCausalBasisMissing) {
			t.Errorf("%s claim: got %s, want no basis warning", typ, HintCausalBasisMissing)
		}
	}
}

func TestNonCanonicalBasisTypeFailsHard(t *testing.T) {
	// A declared basis outside the canonical vocabulary is a caller
	// error, not a judgement call — unlike the missing-basis warning it
	// refuses the write.
	errs, hints := CheckClaimStructure(domain.Claim{
		Type:  domain.ClaimTypeCausal,
		Basis: []domain.ClaimBasis{{Type: "rct"}},
	})
	if len(errs) == 0 {
		t.Fatal("non-canonical basis type: errs = none, want a hard failure")
	}
	if len(hints) != 0 {
		t.Errorf("non-canonical basis: hints = %+v, want none (a hard failure is not a warning)", hints)
	}
}

func TestQuantitativeScopeUnstructuredWarns(t *testing.T) {
	cases := []struct {
		name  string
		scope json.RawMessage
	}{
		{"no scope at all", nil},
		{"free-text string scope", json.RawMessage(`"298 K, 1 bar"`)},
		{"prose-only object scope", json.RawMessage(`{"note":"measured at room temperature"}`)},
		{"population-only scope", json.RawMessage(`{"population":"as-synthesized powder"}`)},
	}
	for _, tc := range cases {
		errs, hints := CheckClaimStructure(domain.Claim{
			Type:  domain.ClaimTypeQuantitative,
			Scope: tc.scope,
		})
		if len(errs) != 0 {
			t.Errorf("%s: errs = %v, want none", tc.name, errs)
		}
		if !hasHintCode(t, hints, HintQuantitativeScopeUnstructured) {
			t.Errorf("%s: hints = %+v, want %s", tc.name, hints, HintQuantitativeScopeUnstructured)
		}
	}
}

func TestQuantitativeScopeStructuredDoesNotWarn(t *testing.T) {
	cases := []struct {
		name  string
		scope json.RawMessage
	}{
		{"conditions", json.RawMessage(`{"conditions":[{"name":"temperature","value":298,"unit":"K"}]}`)},
		{"statistics", json.RawMessage(`{"statistics":{"n":3,"uncertainty":{"kind":"std","value":0.4,"unit":"cm3/g"}}}`)},
		{"conditions and statistics", json.RawMessage(`{"population":"bulk MOF-5","conditions":[{"name":"pressure","value":1,"unit":"bar"}],"statistics":{"n":3}}`)},
	}
	for _, tc := range cases {
		errs, hints := CheckClaimStructure(domain.Claim{
			Type:  domain.ClaimTypeQuantitative,
			Scope: tc.scope,
		})
		if len(errs) != 0 {
			t.Errorf("%s: errs = %v, want none", tc.name, errs)
		}
		if hasHintCode(t, hints, HintQuantitativeScopeUnstructured) {
			t.Errorf("%s: got %s, want no scope warning", tc.name, HintQuantitativeScopeUnstructured)
		}
	}
}

func TestNonQuantitativeScopeNeverWarns(t *testing.T) {
	// The scope-structure hint is a quantitative-claim concern only: a
	// descriptive claim with a prose scope is perfectly well-formed.
	errs, hints := CheckClaimStructure(domain.Claim{
		Type:  domain.ClaimTypeDescriptive,
		Scope: json.RawMessage(`"under ambient conditions"`),
	})
	if len(errs) != 0 {
		t.Errorf("errs = %v, want none", errs)
	}
	if hasHintCode(t, hints, HintQuantitativeScopeUnstructured) {
		t.Error("descriptive claim with prose scope warned about scope structure, want no warning")
	}
}

func TestCheckClaimPayloadWiresStructureChecks(t *testing.T) {
	// The payload-level check (the object write path) must surface the
	// structure warnings for the fields the payload carries: a causal
	// claim payload has no basis channel, so it always warns — which is
	// exactly the docs/10 §5 missing-declaration case.
	_, hints := Check("claim", "", map[string]any{
		"statement":  "pressure swing increases CO2 capacity.",
		"claim_type": "causal",
	})
	if !hasHintCode(t, hints, HintCausalBasisMissing) {
		t.Errorf("causal claim payload: hints = %+v, want %s", hints, HintCausalBasisMissing)
	}

	// A quantitative claim payload with a structured scope must not warn
	// about structure.
	_, hints = Check("claim", "", map[string]any{
		"statement":  "MOF-5 has a BET surface area of 3800 m2/g.",
		"claim_type": "quantitative",
		"scope": map[string]any{
			"conditions": []any{map[string]any{"name": "temperature", "value": 77.0, "unit": "K"}},
		},
	})
	if hasHintCode(t, hints, HintQuantitativeScopeUnstructured) {
		t.Errorf("structured quantitative payload: hints = %+v, want no scope warning", hints)
	}

	// A payload without a claim_type is none of the structure checks'
	// business (the schema requires claim_type, but the checks must not
	// fire on a payload that does not name a type).
	_, hints = Check("claim", "", map[string]any{"statement": "MOF-5 has a BET surface area of 3800 m2/g."})
	if hasHintCode(t, hints, HintCausalBasisMissing) || hasHintCode(t, hints, HintQuantitativeScopeUnstructured) {
		t.Errorf("typeless claim payload: hints = %+v, want no structure warnings", hints)
	}
}
