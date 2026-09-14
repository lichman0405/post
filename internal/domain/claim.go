package domain

import (
	"encoding/json"
	"fmt"
)

// This file is the structured Claim model (T0502): one independently
// judgeable proposition with a type, a subject/property/value triple, a
// scope, a causal basis and an assessment (docs/08 §Claim, docs/10 §5,
// docs/21 §7, docs/43). The canonical wire shape is the claim schema
// (specs/schemas/claim.schema.json, mirrored into internal/rsg/schemareg);
// this package adds the typed, queryable structure the schema's free-form
// value/scope objects do not fix, and the basis field the schema does not
// carry yet (it lives on the claims projection — infra/migrations/00041 —
// until the schema gains it; see T0502 follow-ups).

// ClaimType names the kind of proposition a claim is. The values are the
// canonical enum of specs/schemas/claim.schema.json properties.claim_type
// (docs/08 §Claim).
type ClaimType string

const (
	// ClaimTypeDescriptive states what something is or how it behaves,
	// without numbers or comparison.
	ClaimTypeDescriptive ClaimType = "descriptive"
	// ClaimTypeQuantitative states a measured or computed quantity.
	ClaimTypeQuantitative ClaimType = "quantitative"
	// ClaimTypeComparative states an ordering or difference between two
	// or more subjects.
	ClaimTypeComparative ClaimType = "comparative"
	// ClaimTypeAssociational states that two observables co-vary,
	// without asserting causation (CLAUDE.md §9.12: correlation is never
	// promoted to causation automatically).
	ClaimTypeAssociational ClaimType = "associational"
	// ClaimTypePredictive states what will be observed under conditions
	// not yet measured.
	ClaimTypePredictive ClaimType = "predictive"
	// ClaimTypeCausal states that one thing causes another. docs/10 §5:
	// a causal claim must declare its causal basis.
	ClaimTypeCausal ClaimType = "causal"
	// ClaimTypeMechanistic states the mechanism through which an effect
	// arises. docs/10 §5: like causal claims, must declare its basis.
	ClaimTypeMechanistic ClaimType = "mechanistic"
)

// ValidClaimType reports whether s is one of the seven canonical claim
// types.
func ValidClaimType(s string) bool {
	switch s {
	case "descriptive", "quantitative", "comparative", "associational",
		"predictive", "causal", "mechanistic":
		return true
	}
	return false
}

// CausalClaimType reports whether t is a claim type docs/10 §5 requires a
// causal basis for: causal and mechanistic.
func CausalClaimType(t ClaimType) bool {
	return t == ClaimTypeCausal || t == ClaimTypeMechanistic
}

// ClaimAssessment is the reviewer assessment of a claim. The values are
// the canonical enum of the claim schema. docs/43: these are NOT a linear
// upgrade ladder — an assessment is a position, not a score, and no Truth
// Score is ever derived from it (CLAUDE.md §9.13).
type ClaimAssessment string

const (
	ClaimAssessmentPreliminary ClaimAssessment = "preliminary"
	ClaimAssessmentSupported   ClaimAssessment = "supported"
	ClaimAssessmentContested   ClaimAssessment = "contested"
	ClaimAssessmentUnresolved  ClaimAssessment = "unresolved"
	ClaimAssessmentSuperseded  ClaimAssessment = "superseded"
	ClaimAssessmentAborted     ClaimAssessment = "aborted"
)

// ValidClaimAssessment reports whether s is one of the six canonical
// claim assessments.
func ValidClaimAssessment(s string) bool {
	switch s {
	case "preliminary", "supported", "contested", "unresolved",
		"superseded", "aborted":
		return true
	}
	return false
}

// ClaimBasisType names one canonical causal basis a causal/mechanistic
// claim can declare (docs/10 §5: controlled intervention, temporal
// ordering, confounders considered, dose-response, mechanistic
// characterization, computational intervention).
type ClaimBasisType string

const (
	// ClaimBasisControlledIntervention: the cause was varied by a
	// controlled intervention (e.g. a randomized experiment).
	ClaimBasisControlledIntervention ClaimBasisType = "controlled_intervention"
	// ClaimBasisTemporalOrdering: the cause demonstrably precedes the
	// effect.
	ClaimBasisTemporalOrdering ClaimBasisType = "temporal_ordering"
	// ClaimBasisConfoundersConsidered: plausible confounders were
	// identified and accounted for.
	ClaimBasisConfoundersConsidered ClaimBasisType = "confounders_considered"
	// ClaimBasisDoseResponse: the effect tracks the dose of the cause.
	ClaimBasisDoseResponse ClaimBasisType = "dose_response"
	// ClaimBasisMechanisticCharacterization: the mechanism linking cause
	// and effect is characterized.
	ClaimBasisMechanisticCharacterization ClaimBasisType = "mechanistic_characterization"
	// ClaimBasisComputationalIntervention: the cause was varied in a
	// computational experiment.
	ClaimBasisComputationalIntervention ClaimBasisType = "computational_intervention"
)

// ValidClaimBasisType reports whether s is one of the six canonical
// causal basis types.
func ValidClaimBasisType(s string) bool {
	switch s {
	case "controlled_intervention", "temporal_ordering",
		"confounders_considered", "dose_response",
		"mechanistic_characterization", "computational_intervention":
		return true
	}
	return false
}

// ClaimBasis is one declared causal basis entry: a canonical basis type
// plus an optional free-text detail naming the concrete evidence (e.g.
// which intervention, which dose-response study).
type ClaimBasis struct {
	Type   ClaimBasisType `json:"type"`
	Detail string         `json:"detail,omitempty"`
}

// ClaimScope is the structured claim scope (docs/21 §7): the qualifiers
// that bound where the claim holds, expressed as machine-readable data —
// a population description, normalized conditions (temperature, pressure,
// ...) and the statistical summary of the quantitative evidence. The
// schema's scope object is free-form; this shape is what "quantitative
// scope 可结构化" means: a scope that parses into this struct can be
// filtered, compared and indexed without free-text parsing.
type ClaimScope struct {
	// Population describes what the claim's scope applies to (material
	// form, sample set, system under study).
	Population string `json:"population,omitempty"`
	// Conditions are the normalized scope qualifiers (docs/21 §7:
	// temperature, pressure, ...). They are the indexable common fields
	// of the claims projection (claims.scope_conditions).
	Conditions []ScopeCondition `json:"conditions,omitempty"`
	// Statistics carries the quantitative summary of the evidence the
	// scope bounds: sample size and the stated uncertainty.
	Statistics *ScopeStatistics `json:"statistics,omitempty"`
}

// ScopeCondition is one normalized scope qualifier: a named physical
// condition with a numeric value and a unit.
type ScopeCondition struct {
	// Name is the condition's canonical name (e.g. "temperature",
	// "pressure").
	Name string `json:"name"`
	// Value is the numeric condition value.
	Value float64 `json:"value"`
	// Unit is the condition's unit (e.g. "K", "bar"); empty when the
	// condition is dimensionless or the unit is stated in Name.
	Unit string `json:"unit,omitempty"`
}

// ScopeStatistics is the quantitative summary a structured quantitative
// scope carries: how much evidence and how precise.
type ScopeStatistics struct {
	// N is the number of independent observations or replicates.
	N int `json:"n,omitempty"`
	// Uncertainty states the precision of the claim's value (std, sem,
	// confidence interval half-width, ...).
	Uncertainty *ScopeUncertainty `json:"uncertainty,omitempty"`
}

// ScopeUncertainty is one stated uncertainty bound.
type ScopeUncertainty struct {
	// Kind names the uncertainty measure ("std", "sem", "ci95",
	// "range_half", ...).
	Kind string `json:"kind"`
	// Value is the numeric bound in the claim's value unit.
	Value float64 `json:"value"`
	// Unit is the bound's unit; empty inherits the value's unit.
	Unit string `json:"unit,omitempty"`
}

// ParseClaimScope decodes raw into the structured claim scope. ok is
// false when raw is empty, not a JSON object, or carries condition
// entries that do not fit the structured shape (a non-numeric condition
// value, a nameless condition). Unknown keys inside the scope object are
// ignored — the schema allows free-form qualifiers alongside the
// structured ones.
func ParseClaimScope(raw json.RawMessage) (ClaimScope, bool) {
	if len(raw) == 0 {
		return ClaimScope{}, false
	}
	var s ClaimScope
	if err := json.Unmarshal(raw, &s); err != nil {
		return ClaimScope{}, false
	}
	// A JSON null unmarshals into the zero struct without error; the
	// scope must actually be an object.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil || probe == nil {
		return ClaimScope{}, false
	}
	for _, c := range s.Conditions {
		if c.Name == "" {
			return ClaimScope{}, false
		}
	}
	return s, true
}

// Structured reports whether the scope carries at least one
// machine-readable element: a normalized condition or a statistical
// summary. A population string alone is free text, not structure.
func (s ClaimScope) Structured() bool {
	return len(s.Conditions) > 0 || s.Statistics != nil
}

// Claim is the structured claim content: the payload fields the claim
// schema fixes plus the declared causal basis. Value and Scope stay raw
// so no information is lost in the round-trip; Scope can be lifted into
// the structured shape with ParseClaimScope.
type Claim struct {
	// Statement is the claim's proposition (schema: statement).
	Statement string
	// Type is the claim's kind (schema: claim_type).
	Type ClaimType
	// SubjectRef names what the claim is about (schema: subject_ref).
	SubjectRef string
	// Property is the predicate/property the claim asserts about the
	// subject (schema: property).
	Property string
	// Value is the asserted value, if any (schema: value; optional per
	// docs/08).
	Value json.RawMessage
	// Scope is the claim's scope/qualifiers as stored (schema: scope).
	Scope json.RawMessage
	// Basis is the declared causal basis (docs/10 §5). It is not a
	// claim-schema field: it lives on the structured claim and the claims
	// projection (claims.basis).
	Basis []ClaimBasis
	// Assessment is the reviewer position on the claim (schema:
	// assessment).
	Assessment ClaimAssessment
}

// ValidateBasis checks the declared causal basis entries. It returns the
// first malformed entry: an empty or non-canonical basis type. It is the
// structural half of the claim basis check — the "causal claim without a
// basis" warning is a semantic judgement and lives in
// internal/rsg/semantics.
func (c Claim) ValidateBasis() error {
	for _, b := range c.Basis {
		if b.Type == "" || !ValidClaimBasisType(string(b.Type)) {
			return fmt.Errorf("domain: claim basis type %q is not canonical (controlled_intervention, temporal_ordering, confounders_considered, dose_response, mechanistic_characterization, computational_intervention)", b.Type)
		}
	}
	return nil
}
