package states

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lichman0405/post/internal/application/validation"
	"github.com/lichman0405/post/internal/domain"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// Service orchestrates the project state use cases against the Repository
// port. It owns input validation (the port trusts, the service verifies)
// and state hash derivation (the content address is domain semantics,
// computed here before storage); it does NOT authorize — actors and
// project membership checks belong to the consuming API task, which passes
// only resolved identities in.
type Service struct {
	repo Repository
	// guard re-runs the commit's validation gate inside the commit
	// transaction (docs/22 §7: "command 再次 server validate"): the gate
	// named by CommitParams.Gate is checked over the rows as written, and
	// a blocked gate rolls the whole transition back.
	guard *validation.Guard
}

// NewService wires the service. The guard is required: a commit whose gate
// cannot be re-validated server-side is refused at wiring time, never
// silently unguarded.
func NewService(repo Repository, guard *validation.Guard) *Service {
	return &Service{repo: repo, guard: guard}
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
	// Gate names the validation gate the commit is held to (T0207). It is
	// required and must be a commit gate (draft, pr, main): release and
	// asset are snapshot actions, not commits. The guard re-runs the gate
	// server-side inside the commit transaction — whatever the caller
	// validated beforehand is never a reason to skip it.
	Gate rsgvalidation.Gate

	// ResearchPRMerge declares that this commit IS the governed advance of
	// main that docs/09 §3 allows: the Research PR merge (T0409,
	// internal/application/merge). It exists for exactly one rule — T0601's
	// frozen-main gate — and it is the ONLY way a commit can reach the main
	// branch of a project whose main_frozen flag is set.
	//
	// A caller sets it when, and only when, it is running the merge of a
	// review-machine-approved Research PR into that PR's target branch. The
	// merge service is the only such caller in this build, and its package
	// doc states the same invariant from the other side ("the only thing
	// that may advance it is a Research PR merge"). Every other commit —
	// the RSG write commands, and any future writer — leaves it false, and
	// a direct semantic write onto frozen main is refused with
	// *MainFrozenDirectWriteError whatever actor asked for it, owner
	// included (docs/09 §3: even the Owner may only advance main through a
	// PR merge).
	//
	// It is a declaration about WHICH PATH the commit came from, not a
	// permission: it grants nothing on its own, and a project that is not
	// frozen is committed to exactly as before whichever way it is set.
	ResearchPRMerge bool
}

// Commit executes one state transition: validate, derive the content hash,
// then hand the atomic boundary to the adapter together with the caller's
// operation callback. The callback is wrapped with the validation guard:
// after the caller's semantic writes, the commit's gate runs INSIDE the
// transaction over the rows as written (docs/22 §7) — a blocked gate
// returns *rsgvalidation.GateBlockedError and the whole transition rolls
// back. The callback's error passes through unchanged (unwrapped from
// *CommitWriteError): it is the domain outcome of the semantic write, not
// a store failure.
func (s *Service) Commit(ctx context.Context, in CommitParams, write WriteFunc) (domain.ProjectState, domain.StateCommit, error) {
	if write == nil {
		return domain.ProjectState{}, domain.StateCommit{}, fmt.Errorf("%w: write callback is required", ErrValidation)
	}
	if s.guard == nil {
		return domain.ProjectState{}, domain.StateCommit{}, fmt.Errorf("%w: the commit guard is not wired", ErrStore)
	}
	if err := validateCommitParams(in); err != nil {
		return domain.ProjectState{}, domain.StateCommit{}, err
	}
	hash, err := domain.ComputeStateHash(in.BaseStateID, in.Operations)
	if err != nil {
		return domain.ProjectState{}, domain.StateCommit{}, fmt.Errorf("%w: operations are not canonical JSON: %v", ErrValidation, err)
	}
	guarded := func(ctx context.Context, tx Transaction, stateID string) error {
		if err := write(ctx, tx, stateID); err != nil {
			return err
		}
		return s.guard.RequireCommitGate(ctx, tx, in.Gate, validation.CommitFacts{
			ProjectID:     in.ProjectID,
			BranchID:      in.BranchID,
			ActorID:       in.ActorID,
			BaseStateID:   in.BaseStateID,
			ResultStateID: stateID,
			Operations:    in.Operations,
		})
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
		// The frozen-main gate (T0601) is decided by the adapter inside the
		// transaction, so it sees the flag and the write atomically; the
		// caller only declares which path the commit came from.
		ResearchPRMerge: in.ResearchPRMerge,
	}, guarded)
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
	if !rsgvalidation.ValidCommitGate(in.Gate) {
		return fmt.Errorf("%w: gate is required and must be a commit gate (draft, pr, main); release and asset are snapshot actions, not commits", ErrValidation)
	}
	return nil
}

// wrapStoreError keeps the expected domain outcomes (missing state/commit/
// branch, head conflict, content-address collision, validation, frozen
// main) and turns everything else — including an adapter that cannot run
// (e.g. a migration not yet applied) — into ErrStore for the handler, with
// the cause kept for the log. A *CommitWriteError is unwrapped: the
// callback's error is the domain outcome of the semantic write and must
// reach the caller unchanged.
func wrapStoreError(err error) error {
	if err == nil ||
		errors.Is(err, ErrStateNotFound) ||
		errors.Is(err, ErrCommitNotFound) ||
		errors.Is(err, ErrBranchNotFound) ||
		errors.Is(err, ErrStateExists) ||
		errors.Is(err, ErrValidation) ||
		errors.As(err, new(*StateConflictError)) ||
		errors.As(err, new(*BranchNotActiveError)) ||
		// The frozen-main refusal (T0601) is a policy outcome, not a store
		// failure: it must reach the caller with its own wire code
		// (MAIN_FROZEN_DIRECT_WRITE_FORBIDDEN) instead of collapsing into
		// SERVICE_UNAVAILABLE.
		errors.As(err, new(*MainFrozenDirectWriteError)) {
		return err
	}
	var we *CommitWriteError
	if errors.As(err, &we) {
		return we.Err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}
