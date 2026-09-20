package domain

import "time"

// Issue is one row of the issues table (migration 00009: project_id,
// number, issue_type, title, body, state, created_by, created_at).
//
// # What this type deliberately does NOT say
//
// docs/03 §GLOSSARY names an Issue exactly once — "项目希望回答的科研问题，
// 可包含子问题，不等同 Issue" — which is a statement about Research
// Questions, not a definition of an Issue. Nothing in docs/ defines the
// Issue state machine, its relation to scientific objects, or what its
// issue_type vocabulary is; the column is free text in 00009 and no code
// has ever written a row (T0811 is the first caller of CreateIssue).
//
// So this type models the ROW and nothing more. It adds no lifecycle of
// its own — State carries the four values 00009's CHECK already fixes and
// no transition is decided here — and no invented field: a caller names
// the issue_type explicitly (internal/application/discussions refuses a
// blank one rather than defaulting to a word the specifications never
// chose). Giving an Issue semantics is a product decision, and the honest
// state of the tree is that it has not been made.
type Issue struct {
	// ID is the uuid v4 text form (issues.id).
	ID string
	// ProjectID is the research boundary the issue belongs to.
	ProjectID string
	// Number is the per-project issue number (1-based, sequential per
	// project; UNIQUE(project_id, number)).
	Number int64
	// IssueType is the caller-declared kind of issue. It is free text in
	// the canonical schema and is stored verbatim (trimmed, non-blank).
	IssueType string
	// Title is the one-line summary.
	Title string
	// Body is the issue's text.
	Body string
	// State is one of the four values 00009's CHECK admits. A created
	// issue carries the column's own default ('open'); this flow never
	// transitions it.
	State IssueState
	// CreatedBy is the user id of the actor who created the row.
	CreatedBy string
	CreatedAt time.Time
}

// IssueState is the issues.state vocabulary (00009's CHECK: open,
// in_progress, closed, aborted). "aborted" is the non-destructive ending
// CLAUDE.md §9.8 requires (nothing disappears); the column is not
// transitioned by anything in this task.
type IssueState string

const (
	// IssueOpen: the column's default for a created row.
	IssueOpen IssueState = "open"
	// IssueInProgress: work on the issue has started.
	IssueInProgress IssueState = "in_progress"
	// IssueClosed: the issue is done.
	IssueClosed IssueState = "closed"
	// IssueAborted: the issue was abandoned — the ending that keeps the
	// row (CLAUDE.md §9.8), unlike a delete.
	IssueAborted IssueState = "aborted"
)

// ValidIssueState reports whether s is one of the four stored states
// (00009's CHECK).
func ValidIssueState(s IssueState) bool {
	switch s {
	case IssueOpen, IssueInProgress, IssueClosed, IssueAborted:
		return true
	}
	return false
}
