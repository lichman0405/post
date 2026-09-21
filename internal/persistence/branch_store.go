package persistence

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// BranchStore is the production branches.Repository adapter over
// PostgreSQL (sqlc generated queries, pgx).
//
// Creation invariants run in ONE transaction: the project row is read
// (existence, and the visibility preset the default derives from), the
// fork point is re-checked inside the guarded insert (a branch always
// forks a state of the same project), and the row is inserted — a project
// that is missing can never receive a branch, and the default visibility
// is always the true project preset, never a caller-supplied copy.
//
// Lifecycle transitions are compare-and-swaps on the active state
// (docs/43: active → merged | aborted, terminal); main is protected. The
// database additionally rejects any head movement or lifecycle change on a
// closed branch for ANY update path (migration 00028 trigger), so the
// immutability of merged/aborted history holds even against code that
// bypasses this adapter.
type BranchStore struct {
	pool *pgxpool.Pool
}

// NewBranchStore builds the store on pool. The pool may be lazy
// (OpenLazy): the API keeps starting while PostgreSQL is down.
func NewBranchStore(pool *pgxpool.Pool) *BranchStore {
	return &BranchStore{pool: pool}
}

// CreateBranch implements branches.Repository.
func (s *BranchStore) CreateBranch(ctx context.Context, in branches.CreateBranchParams) (domain.Branch, error) {
	projectID, err := textUUID(in.ProjectID)
	if err != nil {
		return domain.Branch{}, branches.ErrValidation
	}
	baseStateID, err := textUUID(in.BaseStateID)
	if err != nil {
		return domain.Branch{}, branches.ErrValidation
	}
	createdBy, err := textUUID(in.CreatedBy)
	if err != nil {
		return domain.Branch{}, branches.ErrValidation
	}
	// Empty visibility is not invalid — it means "default from the project
	// preset" (docs/09 §1), resolved inside the transaction below.
	if in.Visibility != "" && !domain.ValidBranchVisibility(in.Visibility) {
		return domain.Branch{}, branches.ErrValidation
	}
	var created domain.Branch
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		project, err := q.GetProjectByID(ctx, projectID)
		if errors.Is(err, pgx.ErrNoRows) {
			return projects.ErrProjectNotFound
		}
		if err != nil {
			return err
		}
		// Visibility defaulting (docs/09 §1): unset follows the project
		// preset; an explicit value may only stay within it — widening
		// beyond the project preset is the audited publication flow's
		// job, never branch creation's (docs/12 §3).
		visibility := in.Visibility
		if visibility == "" {
			visibility = domain.BranchVisibility(project.Visibility)
		}
		if visibility == domain.BranchVisibilityPublic &&
			project.Visibility == string(domain.VisibilityPrivate) {
			return branches.ErrPublicBranchInPrivateProject
		}
		row, err := q.CreateBranchFromState(ctx, sqlc.CreateBranchFromStateParams{
			ProjectID:   projectID,
			Name:        in.Name,
			Visibility:  string(visibility),
			Purpose:     in.Purpose,
			GitRef:      in.GitRef,
			BaseStateID: baseStateID,
			CreatedBy:   createdBy,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// The guarded insert matched nothing: the fork point does
			// not exist in the project — either missing entirely or a
			// state of another project, the same outcome for both
			// (never leak a foreign project's state existence).
			return branches.ErrBaseStateNotFound
		}
		if err != nil {
			return mapBranchWriteError(err)
		}
		created = branchFromRow(row)
		return nil
	})
	if err != nil {
		if errors.Is(err, branches.ErrBaseStateNotFound) ||
			errors.Is(err, branches.ErrBranchNameTaken) ||
			errors.Is(err, branches.ErrPublicBranchInPrivateProject) ||
			errors.Is(err, branches.ErrValidation) ||
			errors.Is(err, projects.ErrProjectNotFound) {
			return domain.Branch{}, err
		}
		return domain.Branch{}, fmt.Errorf("persistence: create branch: %w", err)
	}
	return created, nil
}

// GetBranch implements branches.Repository.
func (s *BranchStore) GetBranch(ctx context.Context, projectID, branchID string) (domain.Branch, error) {
	pid, err := textUUID(projectID)
	if err != nil {
		return domain.Branch{}, branches.ErrBranchNotFound
	}
	id, err := textUUID(branchID)
	if err != nil {
		return domain.Branch{}, branches.ErrBranchNotFound
	}
	row, err := sqlc.New(s.pool).GetBranchByProjectAndID(ctx, sqlc.GetBranchByProjectAndIDParams{
		ID:        id,
		ProjectID: pid,
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return domain.Branch{}, branches.ErrBranchNotFound
	}
	if err != nil {
		return domain.Branch{}, fmt.Errorf("persistence: get branch: %w", err)
	}
	return branchFromRow(row), nil
}

// ListBranches implements branches.Repository. An unknown project has no
// branches: empty list, not an error (the same read discipline as the
// version logs).
func (s *BranchStore) ListBranches(ctx context.Context, projectID string) ([]domain.Branch, error) {
	id, err := textUUID(projectID)
	if err != nil {
		return nil, nil // cannot name a project; empty, not an error
	}
	rows, err := sqlc.New(s.pool).ListBranchesByProject(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("persistence: list branches: %w", err)
	}
	out := make([]domain.Branch, 0, len(rows))
	for _, row := range rows {
		out = append(out, branchFromRow(row))
	}
	return out, nil
}

// SetBranchLifecycle implements branches.Repository. The transition is a
// compare-and-swap on the active state; the zero-row outcomes are
// distinguished by one read: not in the project (never leak a foreign
// entity), main (protected), or already terminal.
func (s *BranchStore) SetBranchLifecycle(ctx context.Context, projectID, branchID string, to domain.BranchLifecycle) (domain.Branch, error) {
	if !domain.ValidBranchLifecycle(to) || to == domain.BranchLifecycleActive {
		return domain.Branch{}, branches.ErrValidation
	}
	pid, err := textUUID(projectID)
	if err != nil {
		return domain.Branch{}, branches.ErrBranchNotFound
	}
	id, err := textUUID(branchID)
	if err != nil {
		return domain.Branch{}, branches.ErrBranchNotFound
	}
	row, err := sqlc.New(s.pool).SetBranchLifecycle(ctx, sqlc.SetBranchLifecycleParams{
		ID:             id,
		ProjectID:      pid,
		LifecycleState: string(to),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		current, gerr := s.GetBranch(ctx, projectID, branchID)
		if errors.Is(gerr, branches.ErrBranchNotFound) {
			return domain.Branch{}, branches.ErrBranchNotFound
		}
		if gerr != nil {
			return domain.Branch{}, gerr
		}
		if current.Name == domain.MainBranchName {
			return domain.Branch{}, branches.ErrMainProtected
		}
		return domain.Branch{}, &branches.NotActiveError{
			BranchID:  branchID,
			Lifecycle: string(current.Lifecycle),
		}
	}
	if err != nil {
		if isInvalidText(err) {
			return domain.Branch{}, branches.ErrBranchNotFound
		}
		return domain.Branch{}, fmt.Errorf("persistence: set branch lifecycle: %w", err)
	}
	return branchFromRow(row), nil
}

// GetBranchHead implements branches.Repository — the branches.
// base_state_id projection read (docs/21 §5).
func (s *BranchStore) GetBranchHead(ctx context.Context, branchID string) (domain.ProjectState, error) {
	id, err := textUUID(branchID)
	if err != nil {
		return domain.ProjectState{}, branches.ErrBranchNotFound
	}
	branch, err := sqlc.New(s.pool).GetBranchByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return domain.ProjectState{}, branches.ErrBranchNotFound
	}
	if err != nil {
		return domain.ProjectState{}, fmt.Errorf("persistence: get branch head: %w", err)
	}
	if !branch.BaseStateID.Valid {
		return domain.ProjectState{}, branches.ErrStateNotFound
	}
	state, err := sqlc.New(s.pool).GetProjectStateByID(ctx, branch.BaseStateID)
	if errors.Is(err, pgx.ErrNoRows) {
		// The head pointer names a state that does not exist — the
		// projection is corrupt; report the same "no head" outcome, the
		// state chain itself is still readable through the states
		// surface.
		return domain.ProjectState{}, branches.ErrStateNotFound
	}
	if err != nil {
		return domain.ProjectState{}, fmt.Errorf("persistence: get branch head state: %w", err)
	}
	return projectStateFromRow(state), nil
}

// GetBranchProject reads the project a branch belongs to, by the branch's
// own id — the row is the answer, never an input (T0817). The cross-project
// reads that have to judge a branch their caller did not scope (the merge's
// source side, the integrity review's chain boundaries) ask this question
// first, so "which project owns this branch" is read rather than assumed.
// An unknown branch answers branches.ErrBranchNotFound, the same outcome
// every other by-id branch read gives.
func (s *BranchStore) GetBranchProject(ctx context.Context, branchID string) (string, error) {
	id, err := textUUID(branchID)
	if err != nil {
		return "", branches.ErrBranchNotFound
	}
	branch, err := sqlc.New(s.pool).GetBranchByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return "", branches.ErrBranchNotFound
	}
	if err != nil {
		return "", fmt.Errorf("persistence: get branch project: %w", err)
	}
	return pgUUIDToText(branch.ProjectID), nil
}

// mapBranchWriteError turns a failed branch insert into the package
// outcomes: the (project, name) collision is a domain result, everything
// else on the validated input is an adapter failure with the cause kept.
func mapBranchWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return branches.ErrBranchNameTaken
	}
	if errors.As(err, &pgErr) && (pgErr.Code == "23503" || pgErr.Code == "23502" || pgErr.Code == "22P02") || isInvalidText(err) {
		return branches.ErrValidation
	}
	return err
}

// branchFromRow converts a sqlc branches row to the domain value.
func branchFromRow(row sqlc.Branch) domain.Branch {
	return domain.Branch{
		ID:          pgUUIDToText(row.ID),
		ProjectID:   pgUUIDToText(row.ProjectID),
		Name:        row.Name,
		Visibility:  domain.BranchVisibility(row.Visibility),
		Purpose:     row.Purpose,
		GitRef:      row.GitRef,
		BaseStateID: uuidPtr(row.BaseStateID),
		Lifecycle:   domain.BranchLifecycle(row.LifecycleState),
		CreatedBy:   pgUUIDToText(row.CreatedBy),
		CreatedAt:   row.CreatedAt.Time,
	}
}
