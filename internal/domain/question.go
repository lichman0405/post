package domain

// Research Question (research_question): the state of the question's
// scientific progress, as scientific CONTENT carried in the versioned
// payload (research_question.schema.json, question_state enum). A new
// assessment is a new version in the append-only log (docs/21 §4), never
// an in-place UPDATE — like every other content field.
//
// QuestionState is deliberately NOT an Issue state. Issues are project
// coordination (open → closed around work items); question_state is the
// scientific assessment of the question itself. Closing an issue never
// touches a question's state, and nothing in the issue domain may
// derive one from the other (task T0501 acceptance criterion).
//
// The values are also deliberately NOT ordered as a linear progression:
// docs/43 says these assessments are not upgrade levels, and an object's
// lifecycle already allows active → aborted → reopened, so any transition
// between two states is a legitimate new version (migration 00040
// documents the same decision at the database layer).
type QuestionState string

const (
	QuestionStateOpen              QuestionState = "open"
	QuestionStatePartiallyAnswered QuestionState = "partially_answered"
	QuestionStateUnresolved        QuestionState = "unresolved"
	QuestionStateSuperseded        QuestionState = "superseded"
	QuestionStateAborted           QuestionState = "aborted"
)

// ValidQuestionState reports whether s is one of the five canonical
// question states of research_question.schema.json.
func ValidQuestionState(s string) bool {
	switch s {
	case "open", "partially_answered", "unresolved", "superseded", "aborted":
		return true
	}
	return false
}

// CanonicalQuestionStates returns the five states in the order the schema
// enum declares them. The integration drift test pins this list to the
// schema's own enum so a schema change cannot silently outrun the domain
// model.
func CanonicalQuestionStates() []string {
	return []string{"open", "partially_answered", "unresolved", "superseded", "aborted"}
}
