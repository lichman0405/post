package domain

import (
	"strings"
	"time"
)

// Review is one human review decision about a pull request, recorded per
// dimension (docs/09 §5: "Review 状态可以按维度记录，不用单一 approve
// 抹平所有差异"; docs/43: "Scientific/Integrity review 各自状态").
// Canonical table reviews (migration 00061).
//
// A review is a judgment about ONE state: ReviewedStateID pins the exact
// proposed head the reviewer evaluated (the PR's proposed_state_id at
// submission time, derived by the adapter — never caller-supplied). After
// the author updates the head and re-requests review, the next round's
// decisions are new rows on the new head, and stale decisions never leak
// into a later round's projection.
type Review struct {
	// ID is the uuid v4 text form (matches the reviews.id uuid column).
	ID string
	// PullRequestID is the PR the review is about.
	PullRequestID string
	// ReviewerID names the user whose decision this is (docs/04 §3:
	// reviews are attributable — the reviewer is responsible for the
	// recorded judgment).
	ReviewerID string
	// Kind is the review dimension: scientific or integrity.
	Kind ReviewKind
	// Decision is the reviewer's verdict about the reviewed state.
	Decision ReviewDecision
	// ReviewedStateID is the exact proposed head the reviewer evaluated.
	ReviewedStateID string
	// Responsibility is the scientific responsibility label the reviewer
	// acted under (docs/04 §3); resolved by the application's
	// reviewer-responsibility hook, empty when no label was resolved
	// (an unconditional role does not need one).
	Responsibility string
	// Body carries the free-form review reasoning; empty when none.
	Body      string
	CreatedAt time.Time
}

// ReviewKind is one review dimension (reviews.review_kind CHECK,
// migration 00061; docs/09 §5). The two dimensions are recorded
// separately: approving the science says nothing about integrity, and a
// changes_requested in one dimension must not be flattened by an approve
// in the other.
type ReviewKind string

const (
	// ReviewKindScientific judges scientific method/conclusions
	// (docs/09 §5: "Scientific Review：科学方法/结论判断").
	ReviewKindScientific ReviewKind = "scientific"
	// ReviewKindIntegrity judges schema, provenance, dependency, rights,
	// hash, visibility and required fields (docs/09 §5).
	ReviewKindIntegrity ReviewKind = "integrity"
)

// ValidReviewKind reports whether k is one of the documented dimensions.
func ValidReviewKind(k ReviewKind) bool {
	return k == ReviewKindScientific || k == ReviewKindIntegrity
}

// ReviewDecision is one reviewer verdict about the reviewed state
// (reviews.decision CHECK, migration 00061; the PR machine's
// changes_requested/approved vocabulary, docs/43).
type ReviewDecision string

const (
	// ReviewDecisionApproved accepts the reviewed state in this
	// dimension.
	ReviewDecisionApproved ReviewDecision = "approved"
	// ReviewDecisionChangesRequested asks the author to change the
	// proposal in this dimension; the PR-level projection moves the PR
	// to changes_requested (docs/43).
	ReviewDecisionChangesRequested ReviewDecision = "changes_requested"
	// ReviewDecisionComment records feedback without a verdict: the
	// dimension is neither approved nor rejected.
	ReviewDecisionComment ReviewDecision = "comment"
)

// ValidReviewDecision reports whether d is one of the three decisions.
func ValidReviewDecision(d ReviewDecision) bool {
	switch d {
	case ReviewDecisionApproved, ReviewDecisionChangesRequested, ReviewDecisionComment:
		return true
	}
	return false
}

// ValidReviewBody reports whether body is a storable review reasoning:
// empty, or bounded (generous but finite — same discipline as the PR
// body).
func ValidReviewBody(body string) bool {
	return len(body) <= 50_000
}

// ValidReviewResponsibility reports whether label is a storable
// responsibility label: exactly empty (no label resolved), or non-blank
// and bounded (whitespace-only output is normalized away by the service
// before it reaches this check). The vocabulary itself is T0604's
// business (docs/04 §3 lists example labels; projects configure their
// own) — this only bounds the stored text.
func ValidReviewResponsibility(label string) bool {
	if label == "" {
		return true
	}
	t := strings.TrimSpace(label)
	return t != "" && len(t) <= 200
}
