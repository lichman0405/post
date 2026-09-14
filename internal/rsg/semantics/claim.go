package semantics

import (
	"encoding/json"
	"fmt"

	"github.com/lichman0405/post/internal/domain"
)

// Claim structure checks (T0502): the structured claim model's own
// semantic rules, over and above the claim schema —
//
//   - a quantitative claim's scope should be structured
//     (domain.ClaimScope: normalized conditions and/or a statistical
//     summary), so the claim stays queryable and comparable
//     (docs/21 §7, acceptance: quantitative scope 可结构化);
//   - a causal/mechanistic claim must declare its causal basis
//     (docs/10 §5: controlled intervention, temporal ordering, ...);
//     without one it earns a validation warning — advisory, never a
//     refusal, and never an automatic claim-type downgrade (the check
//     cannot judge the evidence, only the declaration's absence).
//
// Both rules warn only: whether a scope is structured "enough" and what
// makes a basis sufficient are scientific calls the check must not make
// for the author (same doctrine as claim atomicity — a hint, not a
// judgement).

// Hint codes of the claim structure checks (stable vocabulary, rendered
// to callers as advisory text alongside the atomicity hint).
const (
	// HintCausalBasisMissing: a causal/mechanistic claim declares no
	// causal basis (docs/10 §5 requires one — the declaration's absence
	// is a validation warning, never a refusal).
	HintCausalBasisMissing = "CAUSAL_BASIS_MISSING"
	// HintQuantitativeScopeUnstructured: a quantitative claim's scope is
	// absent or not structured (domain.ClaimScope), so the claim's
	// quantity cannot be filtered or compared mechanically
	// (docs/21 §7).
	HintQuantitativeScopeUnstructured = "QUANTITATIVE_SCOPE_UNSTRUCTURED"
)

// CheckClaimStructure runs the structured claim checks over a parsed
// claim. It returns the hard failures (a non-canonical declared basis
// type — a caller error, never a judgement call) and the advisory hints
// (missing causal basis, unstructured quantitative scope). The basis is
// part of the structured claim, not of the schema payload: callers that
// only have the payload (the object write path) go through Check with the
// payload, which fills Basis as empty — a causal claim written without a
// basis channel is exactly the case docs/10 §5 warns about.
func CheckClaimStructure(c domain.Claim) (errs []error, hints []Hint) {
	if err := c.ValidateBasis(); err != nil {
		errs = append(errs, fmt.Errorf("claim basis: %w", err))
	}
	if domain.CausalClaimType(c.Type) && len(c.Basis) == 0 {
		hints = append(hints, Hint{
			Code: HintCausalBasisMissing,
			Message: "this " + string(c.Type) + " claim declares no causal basis. " +
				"docs/10 §5: a causal/mechanistic claim must declare the basis for causation — " +
				"controlled intervention, temporal ordering, confounders considered, dose-response, " +
				"mechanistic characterization, computational intervention — " +
				"so reviewers can see what the causation rests on, not just that it is asserted.",
		})
	}
	if c.Type == domain.ClaimTypeQuantitative && !structuredQuantitativeScope(c.Scope) {
		hints = append(hints, Hint{
			Code: HintQuantitativeScopeUnstructured,
			Message: "this quantitative claim's scope is not structured. " +
				"docs/21 §7: express the scope as structured data — normalized conditions " +
				"[{\"name\":\"temperature\",\"value\":298,\"unit\":\"K\"}, ...] and/or a statistical " +
				"summary {\"n\":3,\"uncertainty\":{\"kind\":\"std\",\"value\":0.4}} — " +
				"so the claim's quantity can be filtered, compared and indexed.",
		})
	}
	return errs, hints
}

// structuredQuantitativeScope reports whether a quantitative claim's
// scope parses into the structured shape AND carries at least one
// machine-readable element (a normalized condition or a statistical
// summary). A free-text population string alone is not structure — the
// point of the structured scope is queryability, not prose.
func structuredQuantitativeScope(raw json.RawMessage) bool {
	s, ok := domain.ParseClaimScope(raw)
	if !ok {
		return false
	}
	return s.Structured()
}
