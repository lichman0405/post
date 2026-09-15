package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// This file is the structured Evidence Assertion model (T0504): one
// directed, typed edge from an evidence source to a target knowledge
// object, with both endpoints pinned to exact object versions (docs/10
// §3, §4). The canonical wire shape is the evidence-assertion schema
// (specs/schemas/evidence-assertion.schema.json, mirrored into
// internal/rsg/schemareg); this package adds the typed, queryable
// structure the schema's free-form scope object does not fix, and the
// stance bucket the schema deliberately does not carry (docs/10 §4:
// V1 不自动赋数值权重 — no numeric weight exists anywhere in this
// model, and none is derived from it; CLAUDE.md §9.13).
//
// The Evidence Graph is NOT the Provenance Graph (domain invariant 10):
// provenance answers "where did this come from", evidence answers "why
// does this bear on that proposition". They share nodes but never
// relation semantics (docs/10 §1) — an evidence assertion is stored as
// an evidence_assertions row, never as an RSG relation row.

// EvidenceRelation is one of the nine canonical evidence relation types
// (docs/10 §4, the schema's relation enum): how the evidence bears on the
// target knowledge object.
type EvidenceRelation string

const (
	// EvidenceRelationSupports: the evidence positively supports the
	// target claim or result.
	EvidenceRelationSupports EvidenceRelation = "supports"
	// EvidenceRelationContradicts: the evidence positively contradicts
	// the target claim or result.
	EvidenceRelationContradicts EvidenceRelation = "contradicts"
	// EvidenceRelationConsistentWith: the evidence is consistent with the
	// target claim, without actively supporting it.
	EvidenceRelationConsistentWith EvidenceRelation = "consistent_with"
	// EvidenceRelationInconsistentWith: the evidence is inconsistent with
	// the target claim, without actively contradicting it.
	EvidenceRelationInconsistentWith EvidenceRelation = "inconsistent_with"
	// EvidenceRelationReproduces: the evidence independently reproduces
	// the target result.
	EvidenceRelationReproduces EvidenceRelation = "reproduces"
	// EvidenceRelationFailsToReproduce: the evidence attempted and failed
	// to reproduce the target result.
	EvidenceRelationFailsToReproduce EvidenceRelation = "fails_to_reproduce"
	// EvidenceRelationValidates: the evidence validates the target method
	// or model.
	EvidenceRelationValidates EvidenceRelation = "validates"
	// EvidenceRelationChallenges: the evidence challenges the target
	// claim or result.
	EvidenceRelationChallenges EvidenceRelation = "challenges"
	// EvidenceRelationContextualizes: the evidence situates the target
	// claim — bounds it, explains it, or relates it — without taking a
	// position for or against it.
	EvidenceRelationContextualizes EvidenceRelation = "contextualizes"
)

// ValidEvidenceRelation reports whether s is one of the nine canonical
// evidence relation types.
func ValidEvidenceRelation(s string) bool {
	switch s {
	case "supports", "contradicts", "consistent_with", "inconsistent_with",
		"reproduces", "fails_to_reproduce", "validates", "challenges",
		"contextualizes":
		return true
	}
	return false
}

// CanonicalEvidenceRelations returns the nine relation types in the order
// the schema enum declares them. The integration drift test pins this
// list to the schema's own enum so a schema change cannot silently outrun
// the domain model.
func CanonicalEvidenceRelations() []string {
	return []string{"supports", "contradicts", "consistent_with",
		"inconsistent_with", "reproduces", "fails_to_reproduce",
		"validates", "challenges", "contextualizes"}
}

// EvidenceStance is the transparent stance bucket a relation belongs to:
// supporting, contesting, or neutral. It is a LABEL, not a weight —
// docs/10 §8 permits descriptive labels (limited evidence, mixed
// evidence, actively contested, independently reproduced) produced by
// transparent rules, and forbids an implicit Truth Score; a bucket
// carries no number, no magnitude and no ordering, so it can never be
// summed, averaged or ranked into a score (CLAUDE.md §9.13, docs/10 §4:
// V1 不自动赋数值权重).
type EvidenceStance string

const (
	// EvidenceStanceSupporting: the assertion counts on the side of the
	// target proposition.
	EvidenceStanceSupporting EvidenceStance = "supporting"
	// EvidenceStanceContesting: the assertion counts against the target
	// proposition. Supporting and contesting assertions on the SAME pair
	// coexist by design (task acceptance criterion): the model holds
	// both, nothing collapses them into one number.
	EvidenceStanceContesting EvidenceStance = "contesting"
	// EvidenceStanceNeutral: the assertion takes no position.
	EvidenceStanceNeutral EvidenceStance = "neutral"
)

// RelationStance maps a canonical evidence relation to its stance bucket.
// ok is false for a relation outside the canonical nine. The mapping is
// the transparent rule docs/10 §8 asks for:
//
//	supporting: supports, validates, reproduces, consistent_with
//	contesting: contradicts, challenges, inconsistent_with, fails_to_reproduce
//	neutral:    contextualizes
func RelationStance(r EvidenceRelation) (EvidenceStance, bool) {
	switch r {
	case EvidenceRelationSupports, EvidenceRelationValidates,
		EvidenceRelationReproduces, EvidenceRelationConsistentWith:
		return EvidenceStanceSupporting, true
	case EvidenceRelationContradicts, EvidenceRelationChallenges,
		EvidenceRelationInconsistentWith, EvidenceRelationFailsToReproduce:
		return EvidenceStanceContesting, true
	case EvidenceRelationContextualizes:
		return EvidenceStanceNeutral, true
	}
	return "", false
}

// EvidenceType names the kind of evidence source the assertion cites
// (docs/10 §3, the schema's evidence_type enum): an experiment, a
// computation, a dataset, a literature unit, an external attestation, or
// something else.
type EvidenceType string

const (
	EvidenceTypeExperimental        EvidenceType = "experimental"
	EvidenceTypeComputational       EvidenceType = "computational"
	EvidenceTypeDataset             EvidenceType = "dataset"
	EvidenceTypeLiterature          EvidenceType = "literature"
	EvidenceTypeExternalAttestation EvidenceType = "external_attestation"
	EvidenceTypeOther               EvidenceType = "other"
)

// ValidEvidenceType reports whether s is one of the six canonical
// evidence types.
func ValidEvidenceType(s string) bool {
	switch s {
	case "experimental", "computational", "dataset", "literature",
		"external_attestation", "other":
		return true
	}
	return false
}

// CanonicalEvidenceTypes returns the six evidence types in the order the
// schema enum declares them. The integration drift test pins this list to
// the schema's own enum.
func CanonicalEvidenceTypes() []string {
	return []string{"experimental", "computational", "dataset", "literature",
		"external_attestation", "other"}
}

// EvidenceDirectness states how directly the evidence bears on the target
// (the schema's directness enum): directly, through an intermediate
// inference, or undeclared.
type EvidenceDirectness string

const (
	EvidenceDirectnessDirect   EvidenceDirectness = "direct"
	EvidenceDirectnessIndirect EvidenceDirectness = "indirect"
	EvidenceDirectnessUnknown  EvidenceDirectness = "unknown"
)

// ValidEvidenceDirectness reports whether s is one of the three canonical
// directness values.
func ValidEvidenceDirectness(s string) bool {
	switch s {
	case "direct", "indirect", "unknown":
		return true
	}
	return false
}

// CanonicalEvidenceDirectness returns the three directness values in the
// order the schema enum declares them. The integration drift test pins
// this list to the schema's own enum.
func CanonicalEvidenceDirectness() []string {
	return []string{"direct", "indirect", "unknown"}
}

// EvidenceInferenceNature states what kind of inference the evidence
// itself establishes (the schema's inference_nature enum): observation,
// association, causation, mechanism, prediction, description, or
// undeclared. docs/10 §5: correlation is never promoted to causation
// automatically (CLAUDE.md §9.12) — the declared nature is what the
// evidence supports, and a causal claim backed only by associational
// evidence earns a warning, never a silent upgrade.
type EvidenceInferenceNature string

const (
	EvidenceInferenceObservational EvidenceInferenceNature = "observational"
	EvidenceInferenceAssociational EvidenceInferenceNature = "associational"
	EvidenceInferenceCausal        EvidenceInferenceNature = "causal"
	EvidenceInferenceMechanistic   EvidenceInferenceNature = "mechanistic"
	EvidenceInferencePredictive    EvidenceInferenceNature = "predictive"
	EvidenceInferenceDescriptive   EvidenceInferenceNature = "descriptive"
	EvidenceInferenceUnknown       EvidenceInferenceNature = "unknown"
)

// ValidEvidenceInferenceNature reports whether s is one of the seven
// canonical inference natures.
func ValidEvidenceInferenceNature(s string) bool {
	switch s {
	case "observational", "associational", "causal", "mechanistic",
		"predictive", "descriptive", "unknown":
		return true
	}
	return false
}

// CanonicalEvidenceInferenceNatures returns the seven inference natures
// in the order the schema enum declares them. The integration drift test
// pins this list to the schema's own enum.
func CanonicalEvidenceInferenceNatures() []string {
	return []string{"observational", "associational", "causal",
		"mechanistic", "predictive", "descriptive", "unknown"}
}

// EvidenceReviewState is the reviewer position on the assertion itself
// (the schema's review_state enum): unreviewed, reviewed, rejected.
// Reviewing the assertion is not reviewing the claim — the assertion
// carries its own state, and rejection is the way a bad assertion leaves
// the picture (nothing disappears; state only evolves, invariant 8).
type EvidenceReviewState string

const (
	EvidenceReviewUnreviewed EvidenceReviewState = "unreviewed"
	EvidenceReviewReviewed   EvidenceReviewState = "reviewed"
	EvidenceReviewRejected   EvidenceReviewState = "rejected"
)

// ValidEvidenceReviewState reports whether s is one of the three
// canonical review states.
func ValidEvidenceReviewState(s string) bool {
	switch s {
	case "unreviewed", "reviewed", "rejected":
		return true
	}
	return false
}

// CanonicalEvidenceReviewStates returns the three review states in the
// order the schema enum declares them. The integration drift test pins
// this list to the schema's own enum.
func CanonicalEvidenceReviewStates() []string {
	return []string{"unreviewed", "reviewed", "rejected"}
}

// EvidenceAssertion is the structured assertion content: the payload
// fields the evidence-assertion schema fixes. Both endpoints are VERSION
// pins (target_version_ref, evidence_version_ref): the assertion names
// exact object versions, so it keeps meaning when either object moves on
// — the evidence graph answers for the versions it saw, not for whatever
// they later became (docs/10 §3).
//
// The storage shape (infra/migrations/00007, hardened by 00058) pins the
// same endpoints as uuid foreign keys to scientific_object_versions with
// ON DELETE RESTRICT: a pinned version cannot disappear while an
// assertion points at it. The ref strings here are the schema's wire
// shape ("<object-id>@<version-no>"); resolving them to storage ids is
// the write path's job.
//
// This struct is an INTERNAL model, not a wire DTO: it deliberately
// carries no json tags, so encoding/json marshals it under Go field
// names (TargetVersionRef, ...) — fine for in-process round trips, and
// nothing serializes it to a client today. If it ever becomes a DTO on a
// JSON surface, add explicit snake_case json tags then, matching the
// schema's field names, and extend the round-trip test to pin them.
type EvidenceAssertion struct {
	// TargetVersionRef names the exact target knowledge object version
	// the assertion is about (schema: target_version_ref).
	TargetVersionRef string
	// EvidenceVersionRef names the exact evidence source version the
	// assertion cites (schema: evidence_version_ref).
	EvidenceVersionRef string
	// Relation is the assertion's relation (schema: relation).
	Relation EvidenceRelation
	// EvidenceType is the evidence source's kind (schema: evidence_type).
	EvidenceType EvidenceType
	// Scope bounds where the assertion holds (schema: scope, free-form
	// object; kept raw so no information is lost in the round-trip).
	Scope json.RawMessage
	// Directness states how directly the evidence bears on the target
	// (schema: directness). Empty means "not declared" — the storage
	// layer defaults it to unknown.
	Directness EvidenceDirectness
	// InferenceNature states what the evidence itself establishes
	// (schema: inference_nature). Empty means "not declared" — the
	// storage layer defaults it to unknown.
	InferenceNature EvidenceInferenceNature
	// ReasoningNote is the author's free-text explanation of the
	// assertion (schema: reasoning_note). It is also where a literature
	// assertion names the specific evidence unit it cites (docs/10 §6:
	// a DOI alone may not support a claim).
	ReasoningNote string
	// ReviewState is the reviewer position on the assertion itself
	// (schema: review_state, required).
	ReviewState EvidenceReviewState
}

// Validate checks the assertion's mechanical structure. It returns every
// problem found (errors.Join), never a judgement call: canonical enums,
// non-empty version pins, the two pins naming different versions, and a
// scope that is an object when present. Whether an assertion is
// scientifically right is the reviewer's call (review_state), not this
// function's — it carries no weight, no score and no opinion.
func (a EvidenceAssertion) Validate() error {
	var errs []error
	target := strings.TrimSpace(a.TargetVersionRef)
	evidence := strings.TrimSpace(a.EvidenceVersionRef)
	if target == "" {
		errs = append(errs, fmt.Errorf("domain: evidence assertion target version pin (target_version_ref) is required"))
	}
	if evidence == "" {
		errs = append(errs, fmt.Errorf("domain: evidence assertion evidence version pin (evidence_version_ref) is required"))
	}
	if target != "" && target == evidence {
		errs = append(errs, fmt.Errorf("domain: evidence assertion pins the same version (%s) as both target and evidence — an assertion is a directed edge between two versions", target))
	}
	if !ValidEvidenceRelation(string(a.Relation)) {
		errs = append(errs, fmt.Errorf("domain: evidence relation %q is not canonical (supports, contradicts, consistent_with, inconsistent_with, reproduces, fails_to_reproduce, validates, challenges, contextualizes)", a.Relation))
	}
	if !ValidEvidenceType(string(a.EvidenceType)) {
		errs = append(errs, fmt.Errorf("domain: evidence type %q is not canonical (experimental, computational, dataset, literature, external_attestation, other)", a.EvidenceType))
	}
	if a.Directness != "" && !ValidEvidenceDirectness(string(a.Directness)) {
		errs = append(errs, fmt.Errorf("domain: evidence directness %q is not canonical (direct, indirect, unknown)", a.Directness))
	}
	if a.InferenceNature != "" && !ValidEvidenceInferenceNature(string(a.InferenceNature)) {
		errs = append(errs, fmt.Errorf("domain: evidence inference nature %q is not canonical (observational, associational, causal, mechanistic, predictive, descriptive, unknown)", a.InferenceNature))
	}
	if !ValidEvidenceReviewState(string(a.ReviewState)) {
		errs = append(errs, fmt.Errorf("domain: evidence review state %q is not canonical (unreviewed, reviewed, rejected)", a.ReviewState))
	}
	if len(a.Scope) > 0 && !jsonObjectScope(a.Scope) {
		errs = append(errs, fmt.Errorf("domain: evidence assertion scope must be a JSON object when present"))
	}
	return errors.Join(errs...)
}

// jsonObjectScope reports whether raw is a JSON object. It is the
// evidence assertion's half of the scope-object rule (the claim scope
// has its own structured parser; the evidence-assertion schema leaves
// scope free-form, so "is an object" is the only mechanical check there
// is).
func jsonObjectScope(raw json.RawMessage) bool {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	// A JSON null unmarshals into the nil map without error.
	return probe != nil
}
