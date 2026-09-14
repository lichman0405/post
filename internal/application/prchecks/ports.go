package prchecks

import (
	"context"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/manifest"
)

// PRReader is the read surface the service resolves the PR through. The
// production implementation is persistence.PullRequestStore.
type PRReader interface {
	// GetPullRequest returns the PR of the project by its number, or the
	// pullrequests package's ErrPullRequestNotFound (also for a PR of
	// another project).
	GetPullRequest(ctx context.Context, projectID string, number int64) (domain.PullRequest, error)
}

// ProjectReader resolves the project row (the owning organization's id the
// policy universe is listed from). The production implementation is
// persistence.ProjectStore.
type ProjectReader interface {
	GetProject(ctx context.Context, projectID string) (domain.Project, error)
}

// StateReader is the branch-chain read surface. The production
// implementation is persistence.StateStore.
type StateReader interface {
	// ListStates returns the branch's state chain (rows whose branch_id is
	// the branch); an unknown branch has no chain (empty, not an error).
	ListStates(ctx context.Context, branchID string) ([]domain.ProjectState, error)
	// ListCommits returns the branch's commit history; an unknown branch
	// has no history (empty, not an error).
	ListCommits(ctx context.Context, branchID string) ([]domain.StateCommit, error)
	// GetState returns one state by id; ErrStateNotFound when the state
	// does not exist.
	GetState(ctx context.Context, stateID string) (domain.ProjectState, error)
}

// BranchHeadReader reads a branch's head (the branches.base_state_id
// projection, docs/21 §5): a branch that has no states of its own has
// never moved, so its head is the only state a pin may name for it. The
// production implementation is persistence.BranchStore.
type BranchHeadReader interface {
	// GetBranchHead returns the branch's current head state. The branches
	// package's ErrBranchNotFound for an unknown branch, and
	// ErrStateNotFound when the branch carries no head or the head pointer
	// names no existing state (both are data outcomes, not adapter
	// failures).
	GetBranchHead(ctx context.Context, branchID string) (domain.ProjectState, error)
}

// ManifestReader is the lineage read surface: the stored-form members of
// one state's full lineage (the manifest snapshot rows). The production
// implementation is persistence.ManifestStore.
type ManifestReader interface {
	GetManifestSnapshot(ctx context.Context, stateID string) (manifest.Snapshot, error)
}

// PolicyLister is the policy read surface: every policy version of one
// scope, the universe a visibility pin may resolve to (docs/12 §5: the
// organization's and the project's histories). The production
// implementation is persistence.PolicyStore.
type PolicyLister interface {
	List(ctx context.Context, scope domain.PolicyScope) ([]domain.PolicyVersion, error)
}
