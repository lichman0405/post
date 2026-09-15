package semantics

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/relationcatalog"
)

func validAssertion() domain.EvidenceAssertion {
	return domain.EvidenceAssertion{
		TargetVersionRef:   "claim-1@1",
		EvidenceVersionRef: "exp-7@2",
		Relation:           domain.EvidenceRelationSupports,
		EvidenceType:       domain.EvidenceTypeExperimental,
		Scope:              json.RawMessage(`{"population":"as-synthesized powder"}`),
		Directness:         domain.EvidenceDirectnessDirect,
		InferenceNature:    domain.EvidenceInferenceCausal,
		ReasoningNote:      "controlled intervention varies the cause",
		ReviewState:        domain.EvidenceReviewUnreviewed,
	}
}

func hintCode(t *testing.T, hints []Hint) string {
	t.Helper()
	if len(hints) == 0 {
		return ""
	}
	if len(hints) > 1 {
		t.Fatalf("expected at most one hint, got %d: %+v", len(hints), hints)
	}
	return hints[0].Code
}

func TestCheckEvidenceAssertionValid(t *testing.T) {
	target := &domain.Claim{Type: domain.ClaimTypeCausal}
	if errs, hints := CheckEvidenceAssertion(validAssertion(), target); len(errs) != 0 || len(hints) != 0 {
		t.Errorf("CheckEvidenceAssertion(valid, causal target) = (%v, %+v), want no errors, no hints", errs, hints)
	}
	// No target context at all is also fine — the checks that need the
	// target stay silent when the caller cannot resolve it.
	if errs, hints := CheckEvidenceAssertion(validAssertion(), nil); len(errs) != 0 || len(hints) != 0 {
		t.Errorf("CheckEvidenceAssertion(valid, nil target) = (%v, %+v), want no errors, no hints", errs, hints)
	}
}

func TestCheckEvidenceAssertionHardErrors(t *testing.T) {
	// The structural errors of domain.Validate surface as hard failures:
	// a non-canonical relation and an empty review state.
	a := validAssertion()
	a.Relation = "disproves"
	a.ReviewState = ""
	errs, _ := CheckEvidenceAssertion(a, nil)
	if len(errs) == 0 {
		t.Fatal("CheckEvidenceAssertion(non-canonical relation, empty review) = no errors, want hard failures")
	}
	joined := errs[0].Error()
	for _, want := range []string{"relation", "review state"} {
		if !strings.Contains(joined, want) {
			t.Errorf("errors %q do not mention %q", joined, want)
		}
	}
}

func TestCheckEvidenceAssertionCausalTargetAssociationHint(t *testing.T) {
	// docs/10 §5: a causal/mechanistic claim with evidence whose declared
	// inference stops short of causation earns the association warning.
	for _, nature := range []domain.EvidenceInferenceNature{
		domain.EvidenceInferenceObservational,
		domain.EvidenceInferenceAssociational,
		domain.EvidenceInferenceDescriptive,
		domain.EvidenceInferencePredictive,
	} {
		a := validAssertion()
		a.InferenceNature = nature
		_, hints := CheckEvidenceAssertion(a, &domain.Claim{Type: domain.ClaimTypeCausal})
		if got := hintCode(t, hints); got != HintEvidenceAssociationOnly {
			t.Errorf("causal target + %s evidence: hint = %q, want %s", nature, got, HintEvidenceAssociationOnly)
		}
	}
}

func TestCheckEvidenceAssertionNoAssociationHintWhenEvidenceReachesCausation(t *testing.T) {
	// Causal/mechanistic evidence against a causal claim is the case that
	// must NOT warn, and an undeclared nature (unknown) has nothing
	// declared to warn about.
	for _, nature := range []domain.EvidenceInferenceNature{
		domain.EvidenceInferenceCausal,
		domain.EvidenceInferenceMechanistic,
		domain.EvidenceInferenceUnknown,
	} {
		a := validAssertion()
		a.InferenceNature = nature
		_, hints := CheckEvidenceAssertion(a, &domain.Claim{Type: domain.ClaimTypeMechanistic})
		if len(hints) != 0 {
			t.Errorf("causal target + %s evidence: hints = %+v, want none", nature, hints)
		}
	}
}

func TestCheckEvidenceAssertionNoAssociationHintForNonCausalTarget(t *testing.T) {
	// A descriptive claim asserted with associational evidence is not the
	// docs/10 §5 case — the warning is about causal claims specifically.
	a := validAssertion()
	a.InferenceNature = domain.EvidenceInferenceAssociational
	for _, typ := range []domain.ClaimType{
		domain.ClaimTypeDescriptive,
		domain.ClaimTypeQuantitative,
		domain.ClaimTypeComparative,
		domain.ClaimTypeAssociational,
		domain.ClaimTypePredictive,
	} {
		_, hints := CheckEvidenceAssertion(a, &domain.Claim{Type: typ})
		if len(hints) != 0 {
			t.Errorf("%s target + associational evidence: hints = %+v, want none", typ, hints)
		}
	}
}

func TestCheckEvidenceAssertionLiteratureUnitHint(t *testing.T) {
	// docs/10 §6: literature evidence must name the specific evidence
	// unit. The V1 schema carries no excerpt field, so the reasoning note
	// is the only place — an empty note earns the warning.
	a := validAssertion()
	a.EvidenceType = domain.EvidenceTypeLiterature
	a.ReasoningNote = ""
	_, hints := CheckEvidenceAssertion(a, nil)
	if got := hintCode(t, hints); got != HintLiteratureEvidenceUnitUnnamed {
		t.Errorf("literature assertion without note: hint = %q, want %s", got, HintLiteratureEvidenceUnitUnnamed)
	}

	// A note gives the author somewhere to name the unit — whether it
	// names a sufficient one is the reviewer's call, not the check's.
	a.ReasoningNote = "figure 3b, 298 K isotherm"
	if _, hints := CheckEvidenceAssertion(a, nil); len(hints) != 0 {
		t.Errorf("literature assertion with note: hints = %+v, want none", hints)
	}

	// Non-literature evidence has no unit-location requirement.
	e := validAssertion()
	e.ReasoningNote = ""
	if _, hints := CheckEvidenceAssertion(e, nil); len(hints) != 0 {
		t.Errorf("experimental assertion without note: hints = %+v, want none", hints)
	}
}

func TestCheckEvidenceAssertionNeverScores(t *testing.T) {
	// The acceptance surface: whatever the inputs — supporting,
	// contradicting, both at once — the check returns errors and hints
	// only. There is no return value that could carry a score (docs/10
	// §4, CLAUDE.md §9.13: no Truth Score is generated anywhere).
	support := validAssertion()
	support.Relation = domain.EvidenceRelationSupports
	refute := validAssertion()
	refute.Relation = domain.EvidenceRelationContradicts
	target := &domain.Claim{Type: domain.ClaimTypeCausal}
	_, supportHints := CheckEvidenceAssertion(support, target)
	_, refuteHints := CheckEvidenceAssertion(refute, target)
	for _, h := range append(supportHints, refuteHints...) {
		if strings.Contains(h.Code, "SCORE") || strings.Contains(h.Code, "WEIGHT") {
			t.Errorf("hint code %q looks like a score surface", h.Code)
		}
	}
}

// TestEvidenceRelationsStayInTheKnowledgeCatalog pins the assertion
// relation vocabulary to the relation catalog (docs/44): every evidence
// relation is a canonical knowledge-category edge, and none of the nine
// may drift into a provenance edge — the Evidence Graph is not the
// Provenance Graph (docs/10 §1, invariant 10).
func TestEvidenceRelationsStayInTheKnowledgeCatalog(t *testing.T) {
	for _, rel := range domain.CanonicalEvidenceRelations() {
		e, ok := relationcatalog.Lookup(rel)
		if !ok {
			t.Errorf("evidence relation %q is not in the relation catalog (docs/44)", rel)
			continue
		}
		if e.Category != relationcatalog.CategoryKnowledge {
			t.Errorf("evidence relation %q has category %s, want knowledge — evidence edges must never become provenance edges (docs/10 §1)", rel, e.Category)
		}
		if e.ProvenanceInference {
			t.Errorf("evidence relation %q participates in provenance inference — the Evidence Graph must not feed provenance derivation (docs/10 §1)", rel)
		}
	}
}
