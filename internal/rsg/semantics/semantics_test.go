package semantics

import (
	"strings"
	"testing"
)

func TestClaimAtomicityIsHintOnly(t *testing.T) {
	// The acceptance criterion in its own words: Claim atomicity is only a
	// hint, never a hard NLP judgement. Every statement below — however
	// compound it reads — must produce NO hard failure.
	statements := []string{
		// Two independently judgeable propositions joined by "and".
		"MOF-5 has a BET surface area of 3800 m2/g and it is stable in water for 24 hours.",
		// A contrast that reviewers could split.
		"The catalyst is active at 300 K, but it deactivates above 400 K.",
		// Three full sentences.
		"The sample is phase-pure. Its density matches the literature. The diffraction pattern shows no impurity peaks.",
		// Two clauses joined by "while".
		"Yield increases with pressure while selectivity stays constant.",
	}
	for _, statement := range statements {
		errs, hints := Check("claim", "", map[string]any{"statement": statement})
		if len(errs) != 0 {
			t.Fatalf("statement %q produced a hard failure %v; atomicity must be hint-only", statement, errs)
		}
		if len(hints) == 0 {
			t.Errorf("compound statement %q produced no hint", statement)
			continue
		}
		if hints[0].Code != HintClaimCompound {
			t.Errorf("hint code = %s, want %s", hints[0].Code, HintClaimCompound)
		}
	}
}

func TestClaimSinglePropositionNoHint(t *testing.T) {
	_, hints := Check("claim", "", map[string]any{"statement": "MOF-5 has a BET surface area of 3800 m2/g."})
	if len(hints) != 0 {
		t.Fatalf("single-proposition claim produced hints %+v, want none", hints)
	}
	// A statement missing entirely produces neither a failure nor a hint:
	// absence is the schema's business (statement is schema-required).
	errs, hints := Check("claim", "", map[string]any{})
	if len(errs) != 0 || len(hints) != 0 {
		t.Fatalf("empty claim: errs=%v hints=%v, want none", errs, hints)
	}
}

func TestResearchQuestionSelfParentHardFailure(t *testing.T) {
	own := "11111111-2222-4333-8444-555555555555"
	errs, hints := Check("research_question", own, map[string]any{
		"statement":          "What is the stability of MOF-5 in water?",
		"parent_question_id": own,
	})
	if len(errs) != 1 || len(hints) != 0 {
		t.Fatalf("self-parent: errs=%v hints=%v, want exactly one hard failure", errs, hints)
	}
	if !strings.Contains(errs[0].Error(), "own parent") {
		t.Fatalf("self-parent error = %q, want the self-parent explanation", errs[0])
	}
	// A different parent is fine; no parent is fine; a non-string parent is
	// the schema's business.
	errs, _ = Check("research_question", own, map[string]any{
		"parent_question_id": "22222222-3333-4333-8444-555555555555",
	})
	if len(errs) != 0 {
		t.Fatalf("foreign parent produced errors %v", errs)
	}
}

func TestResearchQuestionEmptyParentHardFailure(t *testing.T) {
	own := "11111111-2222-4333-8444-555555555555"
	// A present-but-empty parent is as meaningless as an empty hypothesis
	// question_id: "no parent" is expressed by omitting the field, never
	// by an empty string.
	errs, hints := Check("research_question", own, map[string]any{
		"statement":          "What is the stability of MOF-5 in water?",
		"parent_question_id": "   ",
	})
	if len(errs) != 1 || len(hints) != 0 {
		t.Fatalf("empty parent: errs=%v hints=%v, want exactly one hard failure", errs, hints)
	}
	if !strings.Contains(errs[0].Error(), "must not be empty") {
		t.Fatalf("empty parent error = %q, want the must-not-be-empty explanation", errs[0])
	}
}

func TestHypothesisQuestionRef(t *testing.T) {
	// A present-but-empty question_id is mechanically wrong: hard failure.
	errs, _ := Check("hypothesis", "", map[string]any{"question_id": "  "})
	if len(errs) != 1 {
		t.Fatalf("empty question_id: errs=%v, want one hard failure", errs)
	}
	// A named question is fine.
	errs, hints := Check("hypothesis", "", map[string]any{"question_id": "11111111-2222-4333-8444-555555555555"})
	if len(errs) != 0 || len(hints) != 0 {
		t.Fatalf("named question: errs=%v hints=%v, want none", errs, hints)
	}
	// A missing question_id is tolerated (a draft may not know the question
	// yet) and carries the advisory hint.
	errs, hints = Check("hypothesis", "", map[string]any{"statement": "MOF-5 is water-stable."})
	if len(errs) != 0 {
		t.Fatalf("missing question_id produced a hard failure %v; absence must stay a hint", errs)
	}
	if len(hints) != 1 || hints[0].Code != HintHypothesisQuestionRef {
		t.Fatalf("missing question_id hints = %+v, want exactly %s", hints, HintHypothesisQuestionRef)
	}
}

func TestUnknownTypesHaveNoChecks(t *testing.T) {
	for _, typ := range []string{"material", "sample", "calculation", "dataset", "protocol", "finding", "external_reference", "experiment"} {
		errs, hints := Check(typ, "", map[string]any{"name": "x", "statement": "a and b and c."})
		if len(errs) != 0 || len(hints) != 0 {
			t.Fatalf("type %s: errs=%v hints=%v, want no checks (the V1 schemas define no crisp rule for it)", typ, errs, hints)
		}
	}
}
