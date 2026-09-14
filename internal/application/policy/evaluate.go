package policy

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lichman0405/post/internal/domain"
)

// The policy evaluate interface (T0603 requirement): enforcement sites ask
// the evaluator typed governance questions instead of reading policy_json
// themselves — one evaluation contract, whatever engine implements it.
// The contract is fail-closed: an unknown rule key, or a stored value
// that does not match its registered kind, is an error, and callers
// treat errors as default deny (an absent rule is not an error — the
// caller decides the safe default and finds it reported as Found=false).

// Query names one rule to evaluate.
type Query struct {
	// Rule is the rule key (domain.Rule* constants, or a key registered
	// by a later task in domain/policy.go).
	Rule string
}

// Decision is the typed answer for one rule query.
type Decision struct {
	// Found reports whether the policy sets the rule at all. When false,
	// every typed field is its zero value — the caller applies its safe
	// default (e.g. zero required reviewers, unprotected main).
	Found bool
	// Bool is the value of a bool rule (main_protected,
	// public_asset_ip_review).
	Bool bool
	// Int is the value of an int rule (release_min_reviewers,
	// raw_data_retention_days).
	Int int
	// List is the value of a string-list rule (required_schema_profiles).
	List []string
	// Raw is the rule's raw JSON value when Found.
	Raw json.RawMessage
}

// Evaluator answers governance questions from a policy document.
type Evaluator interface {
	Evaluate(ctx context.Context, p domain.Policy, q Query) (Decision, error)
}

// RuleEvaluator is the V1 implementation: typed lookups over the
// registered rule vocabulary. Absent rules answer Found=false; unknown
// rule keys answer domain.ErrUnknownRule (the engine has no semantics
// for them — a later task extends RuleKindOf and this switch together);
// a stored value that does not match its registered kind is an error
// (default deny — the write path validates documents, so this can only
// mean corrupt state).
type RuleEvaluator struct{}

// NewRuleEvaluator returns the V1 evaluator.
func NewRuleEvaluator() *RuleEvaluator { return &RuleEvaluator{} }

// Evaluate implements Evaluator. An unregistered rule key is an error
// BEFORE the lookup: asking for a rule the engine has no semantics for is
// a programming error, never a silently-permissive "absent" answer.
func (e *RuleEvaluator) Evaluate(_ context.Context, p domain.Policy, q Query) (Decision, error) {
	kind := domain.RuleKindOf(q.Rule)
	if kind == domain.RuleKindUnknown {
		return Decision{}, fmt.Errorf("%w: %q has no registered semantics", domain.ErrUnknownRule, q.Rule)
	}
	raw, found := p.Rule(q.Rule)
	if !found {
		return Decision{}, nil
	}
	switch kind {
	case domain.RuleKindBool:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return Decision{}, fmt.Errorf("%w: rule %q must be a boolean", ErrValidation, q.Rule)
		}
		return Decision{Found: true, Bool: b, Raw: raw}, nil
	case domain.RuleKindInt:
		var n int
		if err := json.Unmarshal(raw, &n); err != nil {
			return Decision{}, fmt.Errorf("%w: rule %q must be an integer", ErrValidation, q.Rule)
		}
		return Decision{Found: true, Int: n, Raw: raw}, nil
	case domain.RuleKindStringList:
		var list []string
		if err := json.Unmarshal(raw, &list); err != nil {
			return Decision{}, fmt.Errorf("%w: rule %q must be a list of strings", ErrValidation, q.Rule)
		}
		return Decision{Found: true, List: list, Raw: raw}, nil
	default:
		return Decision{}, fmt.Errorf("%w: %q has no registered semantics", domain.ErrUnknownRule, q.Rule)
	}
}
