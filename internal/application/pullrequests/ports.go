package pullrequests

import (
	"context"

	"github.com/lichman0405/post/internal/domain"
)

// Repository is the persistence port for research pull requests
// (docs/52: application orchestrates against ports; adapters live in
// internal/persistence). PR creation is one atomic adapter call: the
// project existence check (and the per-project number allocation, locked
// on the project row), the branch pair's existence/lifecycle/head reads
// and the insert share one transaction — so the base and proposed states
// are always pinned from the true branch heads at creation time, never
// from caller-supplied copies, and concurrent creations of one project
// never race the number.
//
// The base state is fixed for the row's lifetime: there is deliberately
// NO surface that updates it (acceptance "PR base 不随 main 漂移").
// The proposed state moves only through RefreshProposedState — the one
// adapter path that sets the transaction-scoped flag migration 00051's
// fixity guard requires (acceptance "head update 可显式 refresh").
// State transitions are compare-and-swaps on the expected state
// (docs/43); a missed CAS fails with *StateConflictError. The database
// trigger enforces the same transition map for any write path.
type Repository interface {
	// CreatePullRequest inserts one PR row. It fails with
	// projects.ErrProjectNotFound for an unknown project,
	// ErrBranchNotFound when a branch of the pair does not exist in the
	// project, *BranchNotActiveError when one of them is merged/aborted,
	// ErrBranchHeadMissing when one of them has no head state, and
	// ErrValidation for malformed identities.
	CreatePullRequest(ctx context.Context, in CreatePullRequestParams) (domain.PullRequest, error)
	// GetPullRequest returns the project's PR by number, or
	// ErrPullRequestNotFound (a PR number of another project reports the
	// same outcome — never leak another project's entity existence).
	GetPullRequest(ctx context.Context, projectID string, number int64) (domain.PullRequest, error)
	// ListPullRequests returns every PR of the project, in number order.
	ListPullRequests(ctx context.Context, projectID string) ([]domain.PullRequest, error)
	// SetPullRequestState transitions the PR expected → to through the
	// compare-and-swap. It fails with ErrPullRequestNotFound for an
	// unknown or foreign PR, and *StateConflictError when the row is no
	// longer in the expected state. Reaching merged stamps merged_at.
	SetPullRequestState(ctx context.Context, projectID string, number int64, expected, to domain.PullRequestState) (domain.PullRequest, error)
	// RefreshProposedState re-pins proposed_state_id to the source
	// branch's CURRENT head — the explicit head refresh, and the only
	// path that moves the proposed state. It fails with
	// ErrPullRequestNotFound for an unknown or foreign PR,
	// *TerminalError when the PR is merged/closed/aborted (terminal PRs
	// never move, docs/43), and ErrBranchHeadMissing when the source
	// branch has no head.
	RefreshProposedState(ctx context.Context, projectID string, number int64) (domain.PullRequest, error)
}

// CreatePullRequestParams carries one PR creation request. Number,
// BaseStateID and ProposedStateID are DERIVED by the adapter inside the
// creation transaction (project-locked number allocation; base pinned
// from the target branch's head, proposed from the source branch's
// head) — the service validates the shape, the adapter owns the
// project-scoped atomicity.
type CreatePullRequestParams struct {
	ProjectID string
	// SourceBranchID names the branch the proposed changes live on.
	SourceBranchID string
	// TargetBranchID names the branch the proposal merges into
	// (typically main, docs/09 §3). Must differ from SourceBranchID.
	TargetBranchID string
	// Title is the one-line proposal summary
	// (domain.ValidPullRequestTitle).
	Title string
	// Body carries the free-form proposal context; empty when none
	// (domain.ValidPullRequestBody).
	Body string
	// CreatedBy names the user opening the PR (the consuming API task
	// passes a resolved identity).
	CreatedBy string
}
