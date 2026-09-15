package semantics

import (
	"fmt"

	"github.com/lichman0405/post/internal/domain"
)

// Finding checks (T0503): the finding payload's own semantic rules, over
// and above the finding schema. Everything hinges on claim_version_refs —
// a Finding is the object that aggregates claim VERSIONS (docs/08
// §Finding), so a present refs value is checked hard:
//
//   - present-but-empty is a caller error: it says the finding pins
//     nothing, which contradicts the object ("must not be empty when
//     present" — the rule checkHypothesis's question_id and
//     checkResearchQuestion's parent_question_id already state);
//   - a present value of any other shape is a caller error (not an
//     array, a non-string element, a ref that is not a canonical uuid:
//     it could never name a scientific_object_versions row);
//   - ABSENT is not: a finding that does not pin anything YET is a
//     draft, and the draft allowance belongs to the gate ladder, not to
//     this package. docs/08 §Draft has a draft lack some domain fields;
//     the draft gate reports a schema-incomplete payload instead of
//     refusing it (internal/rsg/validation/spec.go:90, CheckSchemaTyped
//     = SeverityWarning) and demands the full schema only at PR
//     (spec.go:100, SeverityBlocking). checkHypothesis and
//     checkExternalReference make exactly this call for their own
//     missing-field cases. So absence yields a HINT
//     (HintFindingClaimVersionRefs): reported to the caller, never a
//     refusal. A JSON null is the registry's way of saying absent, and
//     stays legal here as it does in checkResearchQuestion.
//
// The PR gate is NOT weakened by this: a ref-less finding still cannot
// merge, and the projection guard (00057's findings_refs_present) still
// refuses a findings row with no refs at COMMIT — the hint is a report,
// not a pass.
//
// Whether each named version exists, is a claim, and sits in the
// finding's project is the database's call (00057's guard), never this
// package's: the checks here never read storage.
//
// Deliberately NOT checked here:
//
//   - finding_type/assessment enum membership: the schema's enums are
//     the authority on the wire values, exactly as claim_type is for
//     claims — a semantics check would duplicate the schema;
//   - negative_finding content rules: what a negative finding must link
//     to beyond its pinned claims is a scientific judgement (docs/44's
//     contradicts/fails_to_reproduce edges are relation-level content),
//     and no payload-level heuristic may block a negative finding write.
//     The type itself is a first-class, unconstrained member of the
//     canonical set.
func checkFinding(payload map[string]any) ([]error, []Hint) {
	var f domain.Finding
	if s, ok := payload["finding_type"].(string); ok {
		f.Type = domain.FindingType(s)
	}
	if s, ok := payload["assessment"].(string); ok {
		f.Assessment = domain.FindingAssessment(s)
	}
	if s, ok := payload["statement"].(string); ok {
		f.Statement = s
	}

	refs, present := payload["claim_version_refs"]
	if !present || refs == nil {
		// Absent (or an explicit JSON null): the draft's business, not
		// this package's. The caller gets the observation, not a refusal.
		return nil, []Hint{{
			Code: HintFindingClaimVersionRefs,
			Message: "the finding does not yet pin any claim version (claim_version_refs). " +
				"docs/08 §Finding: a finding aggregates the claim versions it is built on — a draft may still " +
				"leave the field out (Draft 可缺部分 domain field), entering PR demands it.",
		}}
	}
	list, ok := refs.([]any)
	if !ok {
		return []error{fmt.Errorf("semantics: finding claim_version_refs must be an array of claim version ids")}, nil
	}
	for i, ref := range list {
		s, isString := ref.(string)
		if !isString {
			return []error{fmt.Errorf("semantics: finding claim_version_refs[%d] is not a string claim version id", i)}, nil
		}
		f.ClaimVersionRefs = append(f.ClaimVersionRefs, s)
	}

	// Present refs, however they read: the finding HAS stated what it
	// aggregates, so the statement must be a usable one. The domain
	// validator refuses an empty list and a ref that is not a canonical
	// uuid — the two mechanically decidable halves of docs/08's
	// "contained claim versions".
	if err := f.ValidateClaimVersionRefs(); err != nil {
		return []error{err}, nil
	}
	return nil, nil
}
