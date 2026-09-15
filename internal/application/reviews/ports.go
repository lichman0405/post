package reviews

import (
	"context"

	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
)

// Repository is the persistence port for per-dimension PR reviews
// (docs/52: application orchestrates against ports; adapters live in
// internal/persistence). Review submission is one atomic adapter call:
// the PR row is read project-scoped and locked (existence, lifecycle and
// the proposed head the review evaluates), the review row is inserted
// with that head pinned (reviewed_state_id — never caller-supplied), and
// the changes_requested projection (docs/43: review_required ->
// changes_requested) lands inside the same transaction.
type Repository interface {
	// SubmitReview records one review decision and returns it. It fails
	// with pullrequests.ErrPullRequestNotFound for an unknown or foreign
	// PR, *pullrequests.TerminalError when the PR is merged/closed/
	// aborted (a closed proposal accepts no further review record),
	// ErrAlreadyReviewed when this reviewer already recorded a decision
	// for this dimension on this head, and ErrValidation for malformed
	// identities or an invalid kind/decision.
	SubmitReview(ctx context.Context, in SubmitReviewParams) (domain.Review, error)
	// ListReviews returns every review of the PR, oldest first
	// (created_at, id — a total order).
	ListReviews(ctx context.Context, projectID string, number int64) ([]domain.Review, error)
}

// SubmitReviewParams carries one review submission. ReviewedStateID is
// DERIVED by the adapter from the PR's current proposed head inside the
// submission transaction; Responsibility is resolved by the service
// through the reviewer-responsibility hook (never caller-supplied).
type SubmitReviewParams struct {
	ProjectID      string
	Number         int64
	ReviewerID     string
	Kind           domain.ReviewKind
	Decision       domain.ReviewDecision
	Responsibility string
	Body           string
}

// ProjectGate is the project-surface slice the service needs: the
// membership role is the permission-matrix class input (docs/04 §2).
// The production implementation is projects.Service.
type ProjectGate interface {
	// GetMembership returns the actor's membership, or
	// projects.ErrMemberNotFound when the project is readable but the
	// actor holds no membership; a denied read answers
	// projects.ErrProjectNotFound (existence hiding, docs/45).
	GetMembership(ctx context.Context, actor domain.User, projectID string) (domain.ProjectMembership, error)
}

// ResponsibilityGate is the reviewer-responsibility hook (task
// requirement "reviewer responsibility hook"; docs/04 §3: scientific
// responsibility is for review routing, separate from access roles).
//
// The permission matrix answers submit_scientific_review with
// 'conditional' for viewer/contributor/authenticated-non-member actors:
// this gate resolves the condition — the actor may review only while
// they hold a review responsibility covering the project. The returned
// label is recorded on the review row (the reviewer is attributable for
// the judgment under that responsibility). A maintainer/owner needs no
// resolution: their verdict is unconditional allow, and the gate is
// consulted only to fill the label best-effort.
//
// T0604 lands the rule-based resolver (Research Owners rules by
// object/schema/type); until then production wires nil and the
// conditional verdict fails closed — refusal, never permission by
// default (docs/12).
type ResponsibilityGate interface {
	// ReviewResponsibility resolves the responsibility label the user
	// holds for reviewing in the project: ("", nil) means none; a
	// non-empty label means the actor holds that responsibility. An
	// error means the condition is unknowable — the service fails
	// closed on it.
	ReviewResponsibility(ctx context.Context, projectID, userID string) (string, error)
}

// Deps carries the slices the service composes. Production wiring is in
// cmd/api/main.go: the review store, the projects service (membership),
// the matrix engine, and nil Responsibility until T0604 lands the
// resolver.
type Deps struct {
	Repo     Repository
	Projects ProjectGate
	Authz    authz.Engine
	// Responsibility is the reviewer-responsibility hook; nil fails the
	// conditional verdict closed.
	Responsibility ResponsibilityGate
}
