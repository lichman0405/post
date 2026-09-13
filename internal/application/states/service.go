package states

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lichman0405/post/internal/domain"
)

// Service orchestrates the project state use cases against the Repository
// port. It owns input validation (the port trusts, the service verifies)
// and state hash derivation (the content address is domain semantics,
// computed here before storage); it does NOT authorize — actors and
// project membership checks belong to the consuming API task, which passes
// only resolved identities in.
type Service struct {
	repo Repository
}

// NewService wires the service.
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// CommitParams carries one state transition as requested by a caller (the
// validated, hash-carrying counterpart is the port's CommitStateParams).
type CommitParams struct {
	ProjectID string
	BranchID  string
	ActorID   string
	Via       domain.StateVia
	Message   string
	// Operations is the ordered list of semantic writes the transition
	// consists of; at least one is required — a commit exists to record
	// semantic writes, and a transition without operations is a no-op,
	// not a state change.
	Operations   []domain.StateOperation
	BaseStateID  *string
	GitCommitSHA *string
	// ManifestVersion is the manifest format version the state is written
	// under.
	ManifestVersion string
}

// Commit executes one state transition: validate, derive the content hash,
// then hand the atomic boundary to the adapter together with the caller's
// operation callback. It returns the new state and its commit record. The
// callback's error passes through unchanged (unwrapped from
// *CommitWriteError): it is the domain outcome of the semantic write, not
// a store failure.
func (s *Service) Commit(ctx context.Context, in CommitParams, write WriteFunc) (domain.ProjectState, domain.StateCommit, error) {
	if write == nil {
		return domain.ProjectState{}, domain.StateCommit{}, fmt.Errorf("%w: write callback is required", ErrValidation)
	}
	if err := validateCommitParams(in); err != nil {
		return domain.ProjectState{}, domain.StateCommit{}, err
	}
	hash, err := domain.ComputeStateHash(in.BaseStateID, in.Operations)
	if err != nil {
		return domain.ProjectState{}, domain.StateCommit{}, fmt.Errorf("%w: operations are not canonical JSON: %v", ErrValidation, err)
	}
	state, commit, err := s.repo.CommitState(ctx, CommitStateParams{
		ProjectID:       in.ProjectID,
		BranchID:        in.BranchID,
		ActorID:         in.ActorID,
		Via:             in.Via,
		Message:         in.Message,
		Operations:      in.Operations,
		BaseStateID:     in.BaseStateID,
		StateHash:       hash,
		GitCommitSHA:    in.GitCommitSHA,
		ManifestVersion: in.ManifestVersion,
	}, write)
	if err != nil {
		return domain.ProjectState{}, domain.StateCommit{}, wrapStoreError(err)
	}
	return state, commit, nil
}

// CreateInitialStateParams carries a genesis state creation.
type CreateInitialStateParams struct {
	ProjectID string
	// GitCommitSHA pins the GitProvider commit the repository was
	// provisioned at; nil otherwise.
	GitCommitSHA *string
	// ManifestVersion is the manifest format version the state is written
	// under.
	ManifestVersion string
}

// CreateInitialState creates the project's genesis state (no parent, no
// branch, no commit — the canonical schema cannot represent a commit
// without a branch, and the empty root is not itself a semantic write).
func (s *Service) CreateInitialState(ctx context.Context, in CreateInitialStateParams) (domain.ProjectState, error) {
	if in.ProjectID == "" {
		return domain.ProjectState{}, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if strings.TrimSpace(in.ManifestVersion) == "" {
		return domain.ProjectState{}, fmt.Errorf("%w: manifest_version is required", ErrValidation)
	}
	hash, err := domain.ComputeStateHash(nil, nil)
	if err != nil {
		return domain.ProjectState{}, fmt.Errorf("%w: deriving genesis hash: %v", ErrValidation, err)
	}
	state, err := s.repo.CreateInitialState(ctx, InitialStateParams{
		ProjectID:       in.ProjectID,
		StateHash:       hash,
		GitCommitSHA:    in.GitCommitSHA,
		ManifestVersion: in.ManifestVersion,
	})
	if err != nil {
		return domain.ProjectState{}, wrapStoreError(err)
	}
	return state, nil
}

// GetState returns one state by id, or ErrStateNotFound.
func (s *Service) GetState(ctx context.Context, stateID string) (domain.ProjectState, error) {
	if stateID == "" {
		return domain.ProjectState{}, fmt.Errorf("%w: state_id is required", ErrValidation)
	}
	state, err := s.repo.GetState(ctx, stateID)
	if err != nil {
		return domain.ProjectState{}, wrapStoreError(err)
	}
	return state, nil
}

// GetStateByHash returns the project state with the given content hash, or
// ErrStateNotFound.
func (s *Service) GetStateByHash(ctx context.Context, projectID, stateHash string) (domain.ProjectState, error) {
	if projectID == "" || stateHash == "" {
		return domain.ProjectState{}, fmt.Errorf("%w: project_id and state_hash are required", ErrValidation)
	}
	state, err := s.repo.GetStateByHash(ctx, projectID, stateHash)
	if err != nil {
		return domain.ProjectState{}, wrapStoreError(err)
	}
	return state, nil
}

// GetBranchHead returns the branch's current head state (the
// branches.base_state_id projection), or ErrStateNotFound when the branch
// has no head yet, ErrBranchNotFound when the branch does not exist.
func (s *Service) GetBranchHead(ctx context.Context, branchID string) (domain.ProjectState, error) {
	if branchID == "" {
		return domain.ProjectState{}, fmt.Errorf("%w: branch_id is required", ErrValidation)
	}
	state, err := s.repo.GetBranchHead(ctx, branchID)
	if err != nil {
		return domain.ProjectState{}, wrapStoreError(err)
	}
	return state, nil
}

// ListStates returns the branch's state chain, oldest first.
func (s *Service) ListStates(ctx context.Context, branchID string) ([]domain.ProjectState, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id is required", ErrValidation)
	}
	states, err := s.repo.ListStates(ctx, branchID)
	if err != nil {
		return nil, wrapStoreError(err)
	}
	return states, nil
}

// GetCommit returns one state commit by id, or ErrCommitNotFound.
func (s *Service) GetCommit(ctx context.Context, commitID string) (domain.StateCommit, error) {
	if commitID == "" {
		return domain.StateCommit{}, fmt.Errorf("%w: commit_id is required", ErrValidation)
	}
	commit, err := s.repo.GetCommit(ctx, commitID)
	if err != nil {
		return domain.StateCommit{}, wrapStoreError(err)
	}
	return commit, nil
}

// ListCommits returns the branch's commit history, oldest first.
func (s *Service) ListCommits(ctx context.Context, branchID string) ([]domain.StateCommit, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id is required", ErrValidation)
	}
	commits, err := s.repo.ListCommits(ctx, branchID)
	if err != nil {
		return nil, wrapStoreError(err)
	}
	return commits, nil
}

// ListStateObjectVersions returns the state snapshot's direct scientific
// object version members (rows whose state_id equals the state).
func (s *Service) ListStateObjectVersions(ctx context.Context, stateID string) ([]domain.ScientificObjectVersion, error) {
	if stateID == "" {
		return nil, fmt.Errorf("%w: state_id is required", ErrValidation)
	}
	vs, err := s.repo.ListStateObjectVersions(ctx, stateID)
	if err != nil {
		return nil, wrapStoreError(err)
	}
	return vs, nil
}

// ListStateRelationVersions returns the state snapshot's direct relation
// version members (rows whose state_id equals the state).
func (s *Service) ListStateRelationVersions(ctx context.Context, stateID string) ([]domain.RelationVersion, error) {
	if stateID == "" {
		return nil, fmt.Errorf("%w: state_id is required", ErrValidation)
	}
	vs, err := s.repo.ListStateRelationVersions(ctx, stateID)
	if err != nil {
		return nil, wrapStoreError(err)
	}
	return vs, nil
}

// validateCommitParams checks the transition's shape. The via channel, the
// operation kinds and the operation detail JSON are the domain vocabulary
// checks: unknown channels or kinds are rejected here, before any storage,
// so every stored operation_summary stays parseable and interpretable.
func validateCommitParams(in CommitParams) error {
	if in.ProjectID == "" {
		return fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if in.BranchID == "" {
		return fmt.Errorf("%w: branch_id is required", ErrValidation)
	}
	if in.ActorID == "" {
		return fmt.Errorf("%w: actor_id is required", ErrValidation)
	}
	if !domain.ValidStateVia(in.Via) {
		return fmt.Errorf("%w: via %q is not a canonical channel (web, api, mcp, claude_code, git_compat, system)", ErrValidation, in.Via)
	}
	if strings.TrimSpace(in.Message) == "" {
		return fmt.Errorf("%w: message is required", ErrValidation)
	}
	if len(in.Operations) == 0 {
		return fmt.Errorf("%w: at least one operation is required (a commit records semantic writes)", ErrValidation)
	}
	for i, op := range in.Operations {
		if !domain.ValidStateOperationKind(op.Kind) {
			return fmt.Errorf("%w: operation %d has unknown kind %q", ErrValidation, i, op.Kind)
		}
		if op.EntityID == "" {
			return fmt.Errorf("%w: operation %d (%s) requires entity_id", ErrValidation, i, op.Kind)
		}
		if len(op.Detail) > 0 {
			var shape any
			if err := json.Unmarshal(op.Detail, &shape); err != nil {
				return fmt.Errorf("%w: operation %d (%s) detail must be valid JSON", ErrValidation, i, op.Kind)
			}
		}
	}
	if strings.TrimSpace(in.ManifestVersion) == "" {
		return fmt.Errorf("%w: manifest_version is required", ErrValidation)
	}
	return nil
}

// wrapStoreError keeps the expected domain outcomes (missing state/commit/
// branch, head conflict, content-address collision, validation) and turns
// everything else — including an adapter that cannot run (e.g. a migration
// not yet applied) — into ErrStore for the handler, with the cause kept
// for the log. A *CommitWriteError is unwrapped: the callback's error is
// the domain outcome of the semantic write and must reach the caller
// unchanged.
func wrapStoreError(err error) error {
	if err == nil ||
		errors.Is(err, ErrStateNotFound) ||
		errors.Is(err, ErrCommitNotFound) ||
		errors.Is(err, ErrBranchNotFound) ||
		errors.Is(err, ErrStateExists) ||
		errors.Is(err, ErrValidation) ||
		errors.As(err, new(*StateConflictError)) {
		return err
	}
	var we *CommitWriteError
	if errors.As(err, &we) {
		return we.Err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}
