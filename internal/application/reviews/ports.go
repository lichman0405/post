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
	// Requirement is the required-review calculation the service resolved
	// for this PR (T0604), and it names the head it was computed for
	// (RequiredReviews.HeadStateID). The adapter evaluates it inside the
	// submission transaction and advances review_required -> approved ->
	// merge_ready when it is satisfied — and ONLY when the calculation is
	// about the head the submission is pinned to: a calculation computed
	// before the head moved is stale, the review still records, and the
	// projection advances nothing (fail closed: an advance is decided by
	// an accurate calculation or not at all).
	Requirement domain.RequiredReviews
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

// ResponsibilityGate is the reviewer-responsibility resolver (docs/04
// §3: scientific responsibility is for review routing, separate from
// access roles).
//
// The permission matrix answers submit_scientific_review with
// 'conditional' for viewer/contributor/authenticated-non-member actors:
// this gate resolves the condition — the actor may review only while
// they hold at least one review responsibility in the project (the
// project's Research Owners configuration, migration 00084; T0604 lands
// the rule-based resolver, so production wires the responsibilities
// service instead of nil).
//
// The labels it returns are the PROJECT's labels the actor holds, and
// they buy exactly one thing: the conditional verdict resolves. They are
// not an access grant, they are not consulted by internal/authz, and a
// viewer holding every label in the project is still a viewer — see
// docs/04 §3「不自动赋予更高访问权限」. The service records the label
// that the proposal's routing actually asked for when the reviewer holds
// it (so reviews.responsibility matches the resolved rule), and the
// reviewer's first held label otherwise.
type ResponsibilityGate interface {
	// Responsibilities resolves the responsibility labels the ACTOR holds
	// in the project, sorted; empty means none. It takes the actor, not
	// an id, because resolving them is a project read like any other
	// (the membership gate and the existence-hiding not-found are the
	// resolver's own). An error means the condition is unknowable — the
	// service fails closed on it, never open.
	Responsibilities(ctx context.Context, actor domain.User, projectID string) ([]string, error)
}

// Routing computes the required-review calculation for a proposal (T0604,
// domain.BuildRequiredReviews): which changes it carries, which
// responsibility each is routed to, and which policy rules add
// requirements. The production implementation is
// *responsibilities.Service.
//
// It is a separate port from ResponsibilityGate on purpose: the gate
// answers "may this actor review here?", the routing answers "what does
// this proposal require?". An absent routing fails the projection closed
// (a review records, the state never advances) — the review itself is a
// human judgment and does not depend on the calculation, while the
// advance is decided by it.
type Routing interface {
	// RequiredReviews returns the calculation for the PR's current head.
	RequiredReviews(ctx context.Context, projectID string, number int64) (domain.RequiredReviews, error)
}

// Deps carries the slices the service composes. Production wiring is in
// cmd/api/main.go: the review store, the projects service (membership),
// the matrix engine, and the responsibilities service for both the
// resolver and the required-review calculation.
type Deps struct {
	Repo     Repository
	Projects ProjectGate
	Authz    authz.Engine
	// Responsibility resolves the conditional verdict (nil fails it
	// closed) and supplies the label the review is recorded under.
	Responsibility ResponsibilityGate
	// Routing computes the proposal's required reviews; nil (or an error)
	// records reviews without ever advancing the proposal's state.
	Routing Routing
}
