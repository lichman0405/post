package domain

import (
	"fmt"
	"regexp"
)

// This file is the structured Finding model (T0503): the human-confirmed
// formal object that aggregates claim VERSIONS (docs/08 §Finding: a
// human-readable statement, a finding type, contained claim versions, an
// assessment, a summary scope). The canonical wire shape is the finding
// schema (specs/schemas/finding.schema.json, mirrored into
// internal/rsg/schemareg); this package adds the typed, queryable
// structure the schema's free-form fields do not fix.
//
// The core of the model is ClaimVersionRefs: a finding pins claim
// VERSION ids, never claim object ids. A later edit of a claim is a new
// version row in the append-only log (00014/00015), so the version a
// finding pinned stays the version it saw — a new claim version can never
// silently rewrite an older finding (T0503 acceptance). The projection
// (infra/migrations/00057) pins the same invariant at the database level
// for every write path.

// FindingType names the kind of aggregation a finding is. The values are
// the canonical enum of specs/schemas/finding.schema.json
// properties.finding_type (docs/08 §Finding).
type FindingType string

const (
	// FindingTypeObservation aggregates what was observed, without a
	// comparison or a trend.
	FindingTypeObservation FindingType = "observation"
	// FindingTypeComparison aggregates an ordering or difference between
	// two or more subjects.
	FindingTypeComparison FindingType = "comparison"
	// FindingTypeTrend aggregates an evolution across conditions, time or
	// composition.
	FindingTypeTrend FindingType = "trend"
	// FindingTypeMechanisticInterpretation aggregates an interpretation
	// of the mechanism behind the pinned claims.
	FindingTypeMechanisticInterpretation FindingType = "mechanistic_interpretation"
	// FindingTypeNegative aggregates the negative result: the evidence
	// the pinned claims were NOT supported — a failed reproduction, a
	// contradicted claim, an absence of the expected effect. A negative
	// finding is a first-class finding in the same sense docs/08 gives
	// failed experiments full object status: the negative outcome is
	// legitimate content, not a discarded attempt.
	FindingTypeNegative FindingType = "negative_finding"
	// FindingTypeIntegratedConclusion aggregates the pinned claims into a
	// broader conclusion.
	FindingTypeIntegratedConclusion FindingType = "integrated_conclusion"
)

// CanonicalFindingTypes returns the six finding types in the order the
// schema enum declares them. The integration drift test pins this list to
// the schema's own enum so a schema change cannot silently outrun the
// domain model.
func CanonicalFindingTypes() []string {
	return []string{"observation", "comparison", "trend", "mechanistic_interpretation", "negative_finding", "integrated_conclusion"}
}

// ValidFindingType reports whether s is one of the six canonical finding
// types.
func ValidFindingType(s string) bool {
	switch s {
	case "observation", "comparison", "trend", "mechanistic_interpretation",
		"negative_finding", "integrated_conclusion":
		return true
	}
	return false
}

// Negative reports whether the finding is a negative finding: the
// aggregation of a negative result (the pinned claims were not supported,
// a reproduction failed, an expected effect was absent).
func (t FindingType) Negative() bool {
	return t == FindingTypeNegative
}

// FindingAssessment is the reviewer position on a finding. The values are
// the canonical enum of the finding schema's assessment field (docs/08
// §Finding, docs/43). They are NOT a linear upgrade ladder — an
// assessment is a position, not a score, and no Truth Score is ever
// derived from it (CLAUDE.md §9.13 — the §9 domain invariant 不生成 Truth
// Score / Research Score). Note the deliberate difference from
// claim assessment: a finding is human-confirmed, so its positive
// position is "accepted", while a claim's is "supported" — the two
// vocabularies must not blur (pinned by the unit and integration tests).
type FindingAssessment string

const (
	FindingAssessmentPreliminary FindingAssessment = "preliminary"
	FindingAssessmentAccepted    FindingAssessment = "accepted"
	FindingAssessmentContested   FindingAssessment = "contested"
	FindingAssessmentUnresolved  FindingAssessment = "unresolved"
	FindingAssessmentSuperseded  FindingAssessment = "superseded"
	FindingAssessmentAborted     FindingAssessment = "aborted"
)

// CanonicalFindingAssessments returns the six finding assessments in the
// order the schema enum declares them. The integration drift test pins
// this list to the schema's own enum.
func CanonicalFindingAssessments() []string {
	return []string{"preliminary", "accepted", "contested", "unresolved", "superseded", "aborted"}
}

// ValidFindingAssessment reports whether s is one of the six canonical
// finding assessments.
func ValidFindingAssessment(s string) bool {
	switch s {
	case "preliminary", "accepted", "contested", "unresolved",
		"superseded", "aborted":
		return true
	}
	return false
}

// Finding is the structured finding content: the payload fields the
// finding schema fixes, typed. The schema's summary scope stays in the
// payload; this struct carries what the finding model's rules read.
type Finding struct {
	// Statement is the human-readable statement of the finding (schema:
	// statement).
	Statement string
	// Type is the finding's kind (schema: finding_type).
	Type FindingType
	// ClaimVersionRefs pins the claim versions the finding aggregates
	// (schema: claim_version_refs, required, minItems 1). Each entry is a
	// scientific_object_versions id of a claim object — a VERSION, never
	// an object: the pinned version is immutable, so a later claim
	// version cannot change what this finding aggregated.
	ClaimVersionRefs []string
	// Assessment is the reviewer position on the finding (schema:
	// assessment).
	Assessment FindingAssessment
}

// uuidRe matches the canonical uuid text form (8-4-4-4-12 hex). The
// pinned refs name scientific_object_versions.id rows, whose ids are
// uuids — a ref that cannot be a row id is a caller error, never a
// judgement call.
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidUUID reports whether s is a canonical uuid text form. It checks
// the shape a version id must have, not that the row exists — existence
// is storage's truth, and the database guard (00057) enforces it.
func ValidUUID(s string) bool {
	return uuidRe.MatchString(s)
}

// ValidateClaimVersionRefs checks the pinned claim version refs. It
// returns the first structural problem: no pinned version (the schema
// requires claim_version_refs with minItems 1 — a finding without pinned
// claim versions is not a Finding, docs/08), or a ref that is not a
// canonical uuid (it could never name a version row). Whether the named
// version exists and is a claim is the database's call (00057's guard),
// not this package's.
func (f Finding) ValidateClaimVersionRefs() error {
	if len(f.ClaimVersionRefs) == 0 {
		return fmt.Errorf("domain: finding must pin at least one claim version (claim_version_refs)")
	}
	for _, ref := range f.ClaimVersionRefs {
		if !ValidUUID(ref) {
			return fmt.Errorf("domain: finding claim version ref %q is not a canonical uuid", ref)
		}
	}
	return nil
}
