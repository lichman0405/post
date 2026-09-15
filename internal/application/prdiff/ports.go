package prdiff

import (
	"context"

	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/diff"
)

// Ports (docs/52: application orchestrates against ports; adapters live
// in internal/persistence). The read needs three surfaces: the PR row
// (the two pinned states), the target branch's head (the third state)
// and the diff itself (the T0401 use case).

// PRReader resolves the PR row. The production implementation is
// persistence.PullRequestStore; a missing PR reports the pullrequests
// package's ErrPullRequestNotFound (the port contract, so the service can
// map it).
type PRReader interface {
	// GetPullRequest returns the PR of the project by its number, or
	// pullrequests.ErrPullRequestNotFound.
	GetPullRequest(ctx context.Context, projectID string, number int64) (domain.PullRequest, error)
}

// BranchHeadReader resolves a branch's current head state — the
// three-way diff's target. The production implementation is
// persistence.BranchStore; a branch with no head reports the branches
// package's ErrStateNotFound (also a data outcome, so the service maps it
// rather than failing the read).
type BranchHeadReader interface {
	GetBranchHead(ctx context.Context, branchID string) (domain.ProjectState, error)
}

// Differ computes the three-way diff of three states (the T0401 use
// case). The production implementation is *diffs.Service — this package
// resolves WHICH states, the diff package computes WHAT changed.
type Differ interface {
	Diff(ctx context.Context, in diffs.Params) (*diff.Diff, error)
}
