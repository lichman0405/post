package domain

// Hypothesis assessment (hypothesis.assessment, hypothesis.schema.json):
// where the hypothesis stands scientifically, as content carried in the
// versioned payload. A new assessment is a new version in the append-only
// log (docs/21 §4), never an in-place UPDATE.
//
// HypothesisAssessment is deliberately NOT an Issue state: an issue row
// tracks coordination work and can be closed; an assessment is the
// scientific verdict on the hypothesis itself. They live in different
// tables (issues vs the versioned payload) and nothing derives one from
// the other — closing an issue never changes an assessment (task T0501
// acceptance criterion). Like QuestionState, the values are not a linear
// ladder (docs/43): any transition is a legitimate new version, never an
// illegal one to enforce at the database layer.
type HypothesisAssessment string

const (
	AssessmentProposed  HypothesisAssessment = "proposed"
	AssessmentUnderTest HypothesisAssessment = "under_test"
	AssessmentSupported HypothesisAssessment = "supported"
	AssessmentWeakened  HypothesisAssessment = "weakened"
	AssessmentContested HypothesisAssessment = "contested"
	AssessmentAborted   HypothesisAssessment = "aborted"
)

// ValidHypothesisAssessment reports whether s is one of the six canonical
// assessments of hypothesis.schema.json.
func ValidHypothesisAssessment(s string) bool {
	switch s {
	case "proposed", "under_test", "supported", "weakened", "contested", "aborted":
		return true
	}
	return false
}

// CanonicalHypothesisAssessments returns the six assessments in the order
// the schema enum declares them. The integration drift test pins this
// list to the schema's own enum so a schema change cannot silently outrun
// the domain model.
func CanonicalHypothesisAssessments() []string {
	return []string{"proposed", "under_test", "supported", "weakened", "contested", "aborted"}
}
