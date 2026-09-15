package semantics

import (
	"strings"

	"github.com/lichman0405/post/internal/domain"
)

// Evidence assertion checks (T0504): the assertion model's own semantic
// rules, over and above the evidence-assertion schema —
//
//   - a causal/mechanistic claim asserted with evidence whose declared
//     inference nature stops short of causation (observational,
//     associational, descriptive, predictive) earns a warning — docs/10
//     §5: the agent may warn "the evidence only establishes association",
//     but must never auto-upgrade or auto-downgrade the claim without
//     confirmation (CLAUDE.md §9.12: correlation is never promoted to
//     causation automatically);
//   - a literature assertion without a reasoning note earns a warning —
//     docs/10 §6: a DOI may not support a claim directly; the assertion
//     must name the specific evidence unit (figure, table, results
//     assertion, supplementary dataset, method). The V1 schema has no
//     excerpt field, so the reasoning note is the only place the unit
//     can be named; the check can only see whether that place is empty.
//
// Both rules warn only: whether the evidence genuinely establishes
// causation and whether a note names a sufficient unit are scientific
// calls the check must not make for the author. There is no weight, no
// score and no net-position arithmetic anywhere in this check (docs/10
// §4: V1 不自动赋数值权重; CLAUDE.md §9.13) — an assertion is stored
// with its relation, never folded into a number.

// Hint codes of the evidence assertion checks (stable vocabulary,
// rendered to callers as advisory text).
const (
	// HintEvidenceAssociationOnly: the target is a causal/mechanistic
	// claim but the assertion's declared inference nature stops short of
	// causation — the evidence establishes at most association (docs/10
	// §5).
	HintEvidenceAssociationOnly = "EVIDENCE_ASSOCIATION_ONLY"
	// HintLiteratureEvidenceUnitUnnamed: a literature assertion names no
	// specific evidence unit (docs/10 §6: a DOI alone may not support a
	// claim; figure/table/results assertion/supplementary dataset/method
	// must be located).
	HintLiteratureEvidenceUnitUnnamed = "LITERATURE_EVIDENCE_UNIT_UNNAMED"
)

// CheckEvidenceAssertion runs the assertion checks. It returns the hard
// failures (the structural Validate errors — non-canonical enums, missing
// or identical version pins, a non-object scope) and the advisory hints
// (causal target with sub-causal evidence, unnamed literature unit).
//
// targetClaim names the target's structured claim when the target IS a
// claim; pass nil when it is not (or when the caller cannot resolve it) —
// the causal-evidence hint only fires for a claim target whose type the
// caller actually knows. The checks stay payload-pure: they never read
// storage, and the future evidence write path is expected to call this
// before inserting (the same contract the object write path has with
// Check).
func CheckEvidenceAssertion(a domain.EvidenceAssertion, targetClaim *domain.Claim) (errs []error, hints []Hint) {
	if err := a.Validate(); err != nil {
		errs = append(errs, err)
	}
	if targetClaim != nil && domain.CausalClaimType(targetClaim.Type) &&
		subCausalInference(a.InferenceNature) {
		hints = append(hints, Hint{
			Code: HintEvidenceAssociationOnly,
			Message: "this assertion's evidence declares inference nature " + string(a.InferenceNature) + ", " +
				"which stops short of causation, against a " + string(targetClaim.Type) + " claim. " +
				"docs/10 §5: the evidence only establishes association — the claim's causal basis " +
				"(controlled intervention, temporal ordering, confounders considered, dose-response, " +
				"mechanistic characterization, computational intervention) needs evidence that reaches it. " +
				"Posting this assertion does not change the claim's type or assessment either way.",
		})
	}
	if a.EvidenceType == domain.EvidenceTypeLiterature && strings.TrimSpace(a.ReasoningNote) == "" {
		hints = append(hints, Hint{
			Code: HintLiteratureEvidenceUnitUnnamed,
			Message: "this literature assertion names no specific evidence unit. " +
				"docs/10 §6: a DOI alone may not support a claim — locate the unit " +
				"(figure, table, results assertion, supplementary dataset, method) in the reasoning note, " +
				"so reviewers can check the exact evidence, not just the paper.",
		})
	}
	return errs, hints
}

// subCausalInference reports whether the declared inference nature stops
// short of causation: declared AND below causal/mechanistic. "unknown"
// means undeclared — there is nothing declared to warn about (the
// missing-basis warning is the claim's own check, not the assertion's).
func subCausalInference(n domain.EvidenceInferenceNature) bool {
	switch n {
	case domain.EvidenceInferenceObservational,
		domain.EvidenceInferenceAssociational,
		domain.EvidenceInferenceDescriptive,
		domain.EvidenceInferencePredictive:
		return true
	}
	return false
}
