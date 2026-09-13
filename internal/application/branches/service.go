package branches

import (
	"context"
	"errors"
	"fmt"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
)

// Service orchestrates the research-branch use cases against the
// Repository port. It owns input validation (the port trusts, the service
// verifies) and the git-ref derivation; it does NOT authorize — branch
// creation and lifecycle transitions are project-governance actions whose
// membership/role checks belong to the consuming API task (internal/authz
// already carries ActionCreateBranch), which passes resolved identities
// in.
//
// The state half of the model stays with the states package (T0204):
// committing to a branch, its head advance and its state chain are
// states.Service surface. This service owns the branch row itself — fork,
// visibility, lifecycle, head reads.
type Service struct {
	repo Repository
}

// NewService wires the service.
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// Create forks a new branch from a project state ("branch from base
// state"): validate the shape, derive the git ref, hand the project-scoped
// atomic insert to the adapter. The returned branch has BaseStateID set to
// the fork point — its initial head — and lifecycle active; its head then
// evolves independently through states.Service.Commit.
func (s *Service) Create(ctx context.Context, in CreateBranchParams) (domain.Branch, error) {
	if err := validateCreate(in); err != nil {
		return domain.Branch{}, err
	}
	// The git ref is derived, never caller-supplied: 1:1 with the unique
	// name, so name and ref can never diverge (T0303 publishes the ref).
	in.GitRef = "refs/heads/" + in.Name
	branch, err := s.repo.CreateBranch(ctx, in)
	if err != nil {
		return domain.Branch{}, wrapStoreError(err)
	}
	return branch, nil
}

// Get returns one branch of the project, or ErrBranchNotFound (also for a
// branch of another project — never leak a foreign entity's existence).
func (s *Service) Get(ctx context.Context, projectID, branchID string) (domain.Branch, error) {
	if projectID == "" || branchID == "" {
		return domain.Branch{}, fmt.Errorf("%w: project_id and branch_id are required", ErrValidation)
	}
	branch, err := s.repo.GetBranch(ctx, projectID, branchID)
	if err != nil {
		return domain.Branch{}, wrapStoreError(err)
	}
	return branch, nil
}

// List returns every branch of the project — active, merged and aborted —
// oldest first.
func (s *Service) List(ctx context.Context, projectID string) ([]domain.Branch, error) {
	if projectID == "" {
		return nil, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	branches, err := s.repo.ListBranches(ctx, projectID)
	if err != nil {
		return nil, wrapStoreError(err)
	}
	return branches, nil
}

// Merge closes the branch as merged (docs/43: active → merged): its diff
// became part of the accepted state, its history is immutable from now on.
// The semantic merge itself is the P4 merge engine's job (T0406); this is
// the branch-lifecycle transition it lands. Main is protected — merging
// main is not a branch transition (ErrMainProtected).
func (s *Service) Merge(ctx context.Context, projectID, branchID string) (domain.Branch, error) {
	return s.setLifecycle(ctx, projectID, branchID, domain.BranchLifecycleMerged)
}

// Abort closes the branch as aborted (docs/43: active → aborted): the
// research path is closed without merging, its history stays immutable.
// Main is protected — aborting main is not a branch transition
// (ErrMainProtected).
func (s *Service) Abort(ctx context.Context, projectID, branchID string) (domain.Branch, error) {
	return s.setLifecycle(ctx, projectID, branchID, domain.BranchLifecycleAborted)
}

// setLifecycle runs one lifecycle transition through the adapter's
// compare-and-swap and maps its outcomes.
func (s *Service) setLifecycle(ctx context.Context, projectID, branchID string, to domain.BranchLifecycle) (domain.Branch, error) {
	if projectID == "" || branchID == "" {
		return domain.Branch{}, fmt.Errorf("%w: project_id and branch_id are required", ErrValidation)
	}
	branch, err := s.repo.SetBranchLifecycle(ctx, projectID, branchID, to)
	if err != nil {
		return domain.Branch{}, wrapStoreError(err)
	}
	return branch, nil
}

// GetHead returns the branch's current head state (the branches.
// base_state_id projection, docs/21 §5), or ErrBranchNotFound when the
// branch does not exist, ErrStateNotFound when it has no head yet.
func (s *Service) GetHead(ctx context.Context, branchID string) (domain.ProjectState, error) {
	if branchID == "" {
		return domain.ProjectState{}, fmt.Errorf("%w: branch_id is required", ErrValidation)
	}
	state, err := s.repo.GetBranchHead(ctx, branchID)
	if err != nil {
		return domain.ProjectState{}, wrapStoreError(err)
	}
	return state, nil
}

// validateCreate checks the creation request's shape: identities present,
// name and purpose in the domain shape, visibility one of the canonical
// values or empty (default). The visibility's relation to the project's
// preset is checked by the adapter, which reads the project row inside the
// insert transaction — the service cannot verify it without duplicating
// the read.
func validateCreate(in CreateBranchParams) error {
	if in.ProjectID == "" {
		return fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if !domain.ValidBranchName(in.Name) {
		return fmt.Errorf("%w: %q is not a valid branch name", ErrValidation, in.Name)
	}
	if in.Visibility != "" && !domain.ValidBranchVisibility(in.Visibility) {
		return fmt.Errorf("%w: visibility %q is not public or private", ErrValidation, in.Visibility)
	}
	if !domain.ValidBranchPurpose(in.Purpose) {
		return fmt.Errorf("%w: purpose is blank or oversized", ErrValidation)
	}
	if in.BaseStateID == "" {
		return fmt.Errorf("%w: base_state_id is required (a branch forks an existing project state)", ErrValidation)
	}
	if in.CreatedBy == "" {
		return fmt.Errorf("%w: created_by is required", ErrValidation)
	}
	return nil
}

// wrapStoreError keeps the expected domain outcomes (missing branch/base
// state, name collision, closed lifecycle, protected main, project missing,
// state missing, validation) and turns everything else — including an
// adapter that cannot run — into ErrStore for the handler, with the cause
// kept for the log.
func wrapStoreError(err error) error {
	if err == nil ||
		errors.Is(err, ErrBranchNotFound) ||
		errors.Is(err, ErrBranchNameTaken) ||
		errors.Is(err, ErrBaseStateNotFound) ||
		errors.Is(err, ErrBranchNotActive) ||
		errors.Is(err, ErrMainProtected) ||
		errors.Is(err, ErrPublicBranchInPrivateProject) ||
		errors.Is(err, ErrStateNotFound) ||
		errors.Is(err, ErrValidation) ||
		errors.Is(err, projects.ErrProjectNotFound) ||
		errors.As(err, new(*NotActiveError)) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}
