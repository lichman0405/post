package branches

import (
	"context"

	"github.com/lichman0405/post/internal/domain"
)

// Repository is the persistence port for research branches (docs/52:
// application orchestrates against ports; adapters live in
// internal/persistence). Branch creation is one atomic adapter call: the
// project existence check, the visibility defaulting against the project's
// preset and the insert share one transaction, so a project that is
// deactivated (or never existed) can never receive a branch, and the
// default visibility is always the one true project preset — never a
// caller-supplied copy of it.
//
// Lifecycle transitions are compare-and-swaps on the active state
// (docs/43: active → merged | aborted, terminal). A transition on an
// already-closed branch fails with *NotActiveError; main is protected —
// transitions on it fail with ErrMainProtected. The head advance that
// follows commits is states.Repository.CommitState's surface (T0204), not
// this port's: only commits move heads, and only on active branches.
type Repository interface {
	// CreateBranch inserts one branch row. It fails with
	// projects.ErrProjectNotFound for an unknown project,
	// ErrBaseStateNotFound when BaseStateID does not name a state of the
	// project, ErrBranchNameTaken when the name is taken in the project,
	// ErrPublicBranchInPrivateProject when an explicitly public branch is
	// requested for a private project, and ErrValidation for malformed
	// identities.
	CreateBranch(ctx context.Context, in CreateBranchParams) (domain.Branch, error)
	// GetBranch returns the project's branch by id, or ErrBranchNotFound
	// (a branch of another project reports the same outcome — never leak
	// another project's entity existence).
	GetBranch(ctx context.Context, projectID, branchID string) (domain.Branch, error)
	// ListBranches returns every branch of the project (active, merged and
	// aborted), oldest first.
	ListBranches(ctx context.Context, projectID string) ([]domain.Branch, error)
	// SetBranchLifecycle transitions the branch active → to (merged or
	// aborted; docs/43). It fails with ErrBranchNotFound for an unknown or
	// foreign branch, ErrMainProtected for the project's main branch, and
	// *NotActiveError when the branch is already merged/aborted.
	SetBranchLifecycle(ctx context.Context, projectID, branchID string, to domain.BranchLifecycle) (domain.Branch, error)
	// GetBranchHead returns the branch's current head state (the
	// branches.base_state_id projection, docs/21 §5), or ErrBranchNotFound
	// when the branch does not exist, ErrStateNotFound when it has no head
	// yet.
	GetBranchHead(ctx context.Context, branchID string) (domain.ProjectState, error)
}

// CreateBranchParams carries one branch creation request. The service
// validates shape and derives GitRef; the adapter owns the project-scoped
// atomicity.
type CreateBranchParams struct {
	ProjectID string
	// Name is the branch name (domain.ValidBranchName), unique per
	// project.
	Name string
	// Visibility is the requested visibility. Empty means "default to the
	// project's preset" (docs/09 §1: public project → public, private
	// project → private). An explicit value may only stay within the
	// project's preset — ErrPublicBranchInPrivateProject otherwise.
	Visibility domain.BranchVisibility
	// Purpose optionally states the research-path intent (nil when none).
	Purpose *string
	// BaseStateID names the project state the branch starts from — its
	// initial head, and the base of its first commit ("branch from base
	// state"). Required: every project state belongs to a branch or is the
	// genesis root, so a branch always forks an existing state.
	BaseStateID string
	// CreatedBy names the user creating the branch (the consuming API task
	// passes a resolved identity).
	CreatedBy string
	// GitRef is the git-compat ref, derived by the service as
	// refs/heads/<Name> (1:1 with the unique name); T0303 publishes it.
	GitRef string
}
