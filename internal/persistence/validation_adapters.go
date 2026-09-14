package persistence

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/application/validation"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// ValidationSnapshotRepository is the production validation.SnapshotRepository
// adapter over StateStore: the validate endpoint's snapshot reads. It
// translates the states sentinels (the store's own vocabulary) into the
// validation package's sentinels at the boundary.
type ValidationSnapshotRepository struct {
	store *StateStore
}

// NewValidationSnapshotRepository builds the adapter on the shared state
// store (the same rows the commit path writes).
func NewValidationSnapshotRepository(store *StateStore) *ValidationSnapshotRepository {
	return &ValidationSnapshotRepository{store: store}
}

// BranchProject implements validation.SnapshotRepository. A branch that
// does not exist — or cannot be named as a uuid — is ErrBranchNotFound;
// the caller's project check turns a foreign branch into the same outcome.
func (a *ValidationSnapshotRepository) BranchProject(ctx context.Context, branchID string) (string, error) {
	id, err := textUUID(branchID)
	if err != nil {
		return "", validation.ErrBranchNotFound
	}
	row, err := sqlc.New(a.store.pool).GetBranchByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return "", validation.ErrBranchNotFound
	}
	if err != nil {
		return "", fmt.Errorf("%w: read branch: %v", validation.ErrStore, err)
	}
	return pgUUIDToText(row.ProjectID), nil
}

// GetBranchHead implements validation.SnapshotRepository.
func (a *ValidationSnapshotRepository) GetBranchHead(ctx context.Context, branchID string) (domain.ProjectState, error) {
	state, err := a.store.GetBranchHead(ctx, branchID)
	switch {
	case errors.Is(err, states.ErrBranchNotFound):
		return domain.ProjectState{}, validation.ErrBranchNotFound
	case errors.Is(err, states.ErrStateNotFound):
		return domain.ProjectState{}, validation.ErrStateNotFound
	case errors.Is(err, states.ErrValidation):
		return domain.ProjectState{}, validation.ErrValidation
	case err != nil:
		return domain.ProjectState{}, fmt.Errorf("%w: %v", validation.ErrStore, err)
	}
	return state, nil
}

// ListStates implements validation.SnapshotRepository.
func (a *ValidationSnapshotRepository) ListStates(ctx context.Context, branchID string) ([]domain.ProjectState, error) {
	states, err := a.store.ListStates(ctx, branchID)
	return states, mapListError(err)
}

// ListCommits implements validation.SnapshotRepository.
func (a *ValidationSnapshotRepository) ListCommits(ctx context.Context, branchID string) ([]domain.StateCommit, error) {
	commits, err := a.store.ListCommits(ctx, branchID)
	return commits, mapListError(err)
}

// ListStateObjectVersions implements validation.SnapshotRepository.
func (a *ValidationSnapshotRepository) ListStateObjectVersions(ctx context.Context, stateID string) ([]domain.ScientificObjectVersion, error) {
	vs, err := a.store.ListStateObjectVersions(ctx, stateID)
	return vs, mapListError(err)
}

// ListStateRelationVersions implements validation.SnapshotRepository.
func (a *ValidationSnapshotRepository) ListStateRelationVersions(ctx context.Context, stateID string) ([]domain.RelationVersion, error) {
	vs, err := a.store.ListStateRelationVersions(ctx, stateID)
	return vs, mapListError(err)
}

// mapListError translates the store's list outcomes: the states sentinels
// become the validation ones, everything else is a store failure.
func mapListError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, states.ErrValidation):
		return validation.ErrValidation
	default:
		return fmt.Errorf("%w: %v", validation.ErrStore, err)
	}
}

// ValidationTxProbe is the production validation.TxProbe adapter: the same
// sqlc queries the commit path uses, run through the in-flight transaction
// so the guard sees the rows exactly as the database will keep them.
type ValidationTxProbe struct{}

// NewValidationTxProbe builds the probe (it is stateless — the transaction
// is the state).
func NewValidationTxProbe() *ValidationTxProbe {
	return &ValidationTxProbe{}
}

// ListStatesTx implements validation.TxProbe.
func (p *ValidationTxProbe) ListStatesTx(ctx context.Context, tx validation.TxQuerier, branchID string) ([]domain.ProjectState, error) {
	id, err := textUUID(branchID)
	if err != nil {
		return nil, fmt.Errorf("%w: branch_id: %v", validation.ErrValidation, err)
	}
	rows, err := sqlc.New(tx).ListProjectStatesByBranch(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]domain.ProjectState, 0, len(rows))
	for _, row := range rows {
		out = append(out, projectStateFromRow(row))
	}
	return out, nil
}

// ListCommitsTx implements validation.TxProbe.
func (p *ValidationTxProbe) ListCommitsTx(ctx context.Context, tx validation.TxQuerier, branchID string) ([]domain.StateCommit, error) {
	id, err := textUUID(branchID)
	if err != nil {
		return nil, fmt.Errorf("%w: branch_id: %v", validation.ErrValidation, err)
	}
	rows, err := sqlc.New(tx).ListStateCommitsByBranch(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]domain.StateCommit, 0, len(rows))
	for _, row := range rows {
		out = append(out, stateCommitFromRow(row))
	}
	return out, nil
}

// ListStateObjectVersionsTx implements validation.TxProbe.
func (p *ValidationTxProbe) ListStateObjectVersionsTx(ctx context.Context, tx validation.TxQuerier, stateID string) ([]domain.ScientificObjectVersion, error) {
	id, err := textUUID(stateID)
	if err != nil {
		return nil, fmt.Errorf("%w: state_id: %v", validation.ErrValidation, err)
	}
	rows, err := sqlc.New(tx).ListStateObjectVersionsByState(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]domain.ScientificObjectVersion, 0, len(rows))
	for _, row := range rows {
		out = append(out, versionFromRow(row))
	}
	return out, nil
}

// ListStateRelationVersionsTx implements validation.TxProbe.
func (p *ValidationTxProbe) ListStateRelationVersionsTx(ctx context.Context, tx validation.TxQuerier, stateID string) ([]domain.RelationVersion, error) {
	id, err := textUUID(stateID)
	if err != nil {
		return nil, fmt.Errorf("%w: state_id: %v", validation.ErrValidation, err)
	}
	rows, err := sqlc.New(tx).ListStateRelationVersionsByState(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]domain.RelationVersion, 0, len(rows))
	for _, row := range rows {
		out = append(out, relationVersionFromRow(row))
	}
	return out, nil
}
