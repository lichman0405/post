package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// StateStore is the production states.Repository adapter over PostgreSQL
// (sqlc generated queries, pgx). project_states and state_commits are
// append-only on two layers at once: the port exposes no update path, and
// the database rejects any UPDATE/DELETE of the rows itself (migrations
// 00014/00015).
//
// CommitState is the task's transaction boundary: the result state insert,
// the branch head compare-and-swap (branches.base_state_id), the caller's
// semantic writes and the state commit row all happen in ONE database
// transaction, so a traceable transition is never observable without its
// members and a failed transaction leaves no half state (docs/53: domain
// event and state change share one transaction — here the state change IS
// the event).
type StateStore struct {
	pool *pgxpool.Pool
}

// NewStateStore builds the store on pool. The pool may be lazy
// (OpenLazy): the API keeps starting while PostgreSQL is down.
func NewStateStore(pool *pgxpool.Pool) *StateStore {
	return &StateStore{pool: pool}
}

// CommitState implements states.Repository. The whole transition is one
// WithTx: any failure after the transaction began — a lost
// compare-and-swap, a rejected operation callback, a failing commit
// insert — rolls the state row, the head advance and every row the
// callback wrote back, so no half state can ever be observed.
func (s *StateStore) CommitState(ctx context.Context, in states.CommitStateParams, write states.WriteFunc) (domain.ProjectState, domain.StateCommit, error) {
	projectID, err := textUUID(in.ProjectID)
	if err != nil {
		return domain.ProjectState{}, domain.StateCommit{}, states.ErrValidation
	}
	branchID, err := textUUID(in.BranchID)
	if err != nil {
		return domain.ProjectState{}, domain.StateCommit{}, states.ErrValidation
	}
	actorID, err := textUUID(in.ActorID)
	if err != nil {
		return domain.ProjectState{}, domain.StateCommit{}, states.ErrValidation
	}
	baseUUID, err := optionalUUIDPtr(in.BaseStateID)
	if err != nil {
		return domain.ProjectState{}, domain.StateCommit{}, states.ErrValidation
	}
	var gitCommitSHA *string
	if in.GitCommitSHA != nil && *in.GitCommitSHA != "" {
		gitCommitSHA = in.GitCommitSHA
	}
	opsJSON, err := json.Marshal(in.Operations)
	if err != nil {
		// The service validated the operations; a marshal failure here
		// means the caller bypassed it — reject, never store partials.
		return domain.ProjectState{}, domain.StateCommit{}, fmt.Errorf("%w: operations: %v", states.ErrValidation, err)
	}
	var state domain.ProjectState
	var commit domain.StateCommit
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		// 1. The result state row first: the callback's writes reference
		// it by foreign key, and its UNIQUE(project_id, state_hash)
		// content address rejects an identical transition up front.
		sRow, err := q.CreateProjectState(ctx, sqlc.CreateProjectStateParams{
			ProjectID:       projectID,
			BranchID:        branchID,
			ParentStateID:   baseUUID,
			StateHash:       in.StateHash,
			GitCommitSha:    gitCommitSHA,
			ManifestVersion: in.ManifestVersion,
		})
		if err != nil {
			return err
		}
		stateID := pgUUIDToText(sRow.ID)
		// 2. Advance the branch head through the compare-and-swap: the
		// pointer moves to the new state only while it still equals the
		// base the commit was built on (nil base = the branch has no
		// head yet). Losing interleavings — a concurrent winner, a stale
		// base — leave zero rows here and are distinguished by one read.
		if _, err := q.UpdateBranchBaseState(ctx, sqlc.UpdateBranchBaseStateParams{
			ID:                  branchID,
			ProjectID:           projectID,
			BaseStateID:         sRow.ID,
			ExpectedBaseStateID: baseUUID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return s.branchCASConflict(ctx, q, branchID, projectID, in.BaseStateID)
			}
			return err
		}
		// 3. The semantic writes, inside the same transaction. Their
		// error is the domain outcome of the write — wrap it so the
		// service can pass it through unchanged instead of reporting a
		// store failure.
		if err := write(ctx, tx, stateID); err != nil {
			return &states.CommitWriteError{Err: err}
		}
		// 4. The commit record: actor, via, message, operations, base →
		// result. Last in the transaction, so it exists exactly when
		// everything it describes exists.
		cRow, err := q.CreateStateCommit(ctx, sqlc.CreateStateCommitParams{
			ProjectID:        projectID,
			BranchID:         branchID,
			BaseStateID:      baseUUID,
			ResultStateID:    sRow.ID,
			ActorID:          actorID,
			Via:              string(in.Via),
			Message:          in.Message,
			OperationSummary: opsJSON,
		})
		if err != nil {
			return err
		}
		state = projectStateFromRow(sRow)
		commit = stateCommitFromRow(cRow)
		return nil
	})
	if err != nil {
		return domain.ProjectState{}, domain.StateCommit{}, mapStateWriteError(err)
	}
	return state, commit, nil
}

// branchCASConflict distinguishes the two zero-row CAS outcomes: the
// branch does not exist in the project (never leak another project's
// entity existence — same error either way), or the head moved underneath
// the commit.
func (s *StateStore) branchCASConflict(ctx context.Context, q *sqlc.Queries, branchID, projectID pgtype.UUID, expected *string) error {
	branch, err := q.GetBranchByID(ctx, branchID)
	if errors.Is(err, pgx.ErrNoRows) {
		return states.ErrBranchNotFound
	}
	if err != nil {
		return err
	}
	if branch.ProjectID != projectID {
		// The branch exists but belongs to another project — the same
		// "not found" outcome, never a foreign project's entity
		// existence (docs/45).
		return states.ErrBranchNotFound
	}
	return &states.StateConflictError{
		BranchID: pgUUIDToText(branchID),
		Expected: expected,
		Actual:   uuidPtr(branch.BaseStateID),
	}
}

// CreateInitialState implements states.Repository.
func (s *StateStore) CreateInitialState(ctx context.Context, in states.InitialStateParams) (domain.ProjectState, error) {
	projectID, err := textUUID(in.ProjectID)
	if err != nil {
		return domain.ProjectState{}, states.ErrValidation
	}
	var gitCommitSHA *string
	if in.GitCommitSHA != nil && *in.GitCommitSHA != "" {
		gitCommitSHA = in.GitCommitSHA
	}
	row, err := sqlc.New(s.pool).CreateProjectState(ctx, sqlc.CreateProjectStateParams{
		ProjectID:       projectID,
		StateHash:       in.StateHash,
		GitCommitSha:    gitCommitSHA,
		ManifestVersion: in.ManifestVersion,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		switch {
		case errors.As(err, &pgErr) && pgErr.Code == "23505":
			// UNIQUE(project_id, state_hash): the genesis hash collides —
			// the project already has its root (or an identical state).
			return domain.ProjectState{}, states.ErrStateExists
		case errors.As(err, &pgErr) && (pgErr.Code == "23503" || pgErr.Code == "23502") || isInvalidText(err):
			return domain.ProjectState{}, states.ErrValidation
		default:
			return domain.ProjectState{}, fmt.Errorf("persistence: create initial state: %w", err)
		}
	}
	return projectStateFromRow(row), nil
}

// GetState implements states.Repository.
func (s *StateStore) GetState(ctx context.Context, stateID string) (domain.ProjectState, error) {
	id, err := textUUID(stateID)
	if err != nil {
		return domain.ProjectState{}, states.ErrStateNotFound
	}
	row, err := sqlc.New(s.pool).GetProjectStateByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return domain.ProjectState{}, states.ErrStateNotFound
	}
	if err != nil {
		return domain.ProjectState{}, fmt.Errorf("persistence: get state: %w", err)
	}
	return projectStateFromRow(row), nil
}

// GetStateByHash implements states.Repository.
func (s *StateStore) GetStateByHash(ctx context.Context, projectID, stateHash string) (domain.ProjectState, error) {
	pid, err := textUUID(projectID)
	if err != nil {
		return domain.ProjectState{}, states.ErrStateNotFound
	}
	row, err := sqlc.New(s.pool).GetProjectStateByHash(ctx, sqlc.GetProjectStateByHashParams{
		ProjectID: pid, StateHash: stateHash,
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return domain.ProjectState{}, states.ErrStateNotFound
	}
	if err != nil {
		return domain.ProjectState{}, fmt.Errorf("persistence: get state by hash: %w", err)
	}
	return projectStateFromRow(row), nil
}

// GetBranchHead implements states.Repository.
func (s *StateStore) GetBranchHead(ctx context.Context, branchID string) (domain.ProjectState, error) {
	id, err := textUUID(branchID)
	if err != nil {
		return domain.ProjectState{}, states.ErrBranchNotFound
	}
	branch, err := sqlc.New(s.pool).GetBranchByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return domain.ProjectState{}, states.ErrBranchNotFound
	}
	if err != nil {
		return domain.ProjectState{}, fmt.Errorf("persistence: get branch head: %w", err)
	}
	if !branch.BaseStateID.Valid {
		return domain.ProjectState{}, states.ErrStateNotFound
	}
	return s.GetState(ctx, pgUUIDToText(branch.BaseStateID))
}

// ListStates implements states.Repository. An unknown branch has no chain:
// empty list, not an error (the same read discipline as the version logs).
func (s *StateStore) ListStates(ctx context.Context, branchID string) ([]domain.ProjectState, error) {
	id, err := textUUID(branchID)
	if err != nil {
		return nil, nil // cannot name a chain; empty, not an error
	}
	rows, err := sqlc.New(s.pool).ListProjectStatesByBranch(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("persistence: list states: %w", err)
	}
	out := make([]domain.ProjectState, 0, len(rows))
	for _, row := range rows {
		out = append(out, projectStateFromRow(row))
	}
	return out, nil
}

// GetCommit implements states.Repository.
func (s *StateStore) GetCommit(ctx context.Context, commitID string) (domain.StateCommit, error) {
	id, err := textUUID(commitID)
	if err != nil {
		return domain.StateCommit{}, states.ErrCommitNotFound
	}
	row, err := sqlc.New(s.pool).GetStateCommitByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return domain.StateCommit{}, states.ErrCommitNotFound
	}
	if err != nil {
		return domain.StateCommit{}, fmt.Errorf("persistence: get state commit: %w", err)
	}
	return stateCommitFromRow(row), nil
}

// ListCommits implements states.Repository. An unknown branch has no
// history: empty list, not an error.
func (s *StateStore) ListCommits(ctx context.Context, branchID string) ([]domain.StateCommit, error) {
	id, err := textUUID(branchID)
	if err != nil {
		return nil, nil // cannot name a history; empty, not an error
	}
	rows, err := sqlc.New(s.pool).ListStateCommitsByBranch(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("persistence: list state commits: %w", err)
	}
	out := make([]domain.StateCommit, 0, len(rows))
	for _, row := range rows {
		out = append(out, stateCommitFromRow(row))
	}
	return out, nil
}

// ListStateObjectVersions implements states.Repository — the state
// snapshot's direct object members.
func (s *StateStore) ListStateObjectVersions(ctx context.Context, stateID string) ([]domain.ScientificObjectVersion, error) {
	id, err := textUUID(stateID)
	if err != nil {
		return nil, nil // cannot name a snapshot; empty, not an error
	}
	rows, err := sqlc.New(s.pool).ListStateObjectVersionsByState(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("persistence: list state object versions: %w", err)
	}
	out := make([]domain.ScientificObjectVersion, 0, len(rows))
	for _, row := range rows {
		out = append(out, versionFromRow(row))
	}
	return out, nil
}

// ListStateRelationVersions implements states.Repository — the state
// snapshot's direct relation members.
func (s *StateStore) ListStateRelationVersions(ctx context.Context, stateID string) ([]domain.RelationVersion, error) {
	id, err := textUUID(stateID)
	if err != nil {
		return nil, nil // cannot name a snapshot; empty, not an error
	}
	rows, err := sqlc.New(s.pool).ListStateRelationVersionsByState(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("persistence: list state relation versions: %w", err)
	}
	out := make([]domain.RelationVersion, 0, len(rows))
	for _, row := range rows {
		out = append(out, relationVersionFromRow(row))
	}
	return out, nil
}

// mapStateWriteError turns a failed commit transaction into the package
// outcomes: content-address collisions and dangling references are domain
// results, everything else is an adapter failure with the cause kept.
func mapStateWriteError(err error) error {
	if err == nil ||
		errors.Is(err, states.ErrStateExists) ||
		errors.Is(err, states.ErrBranchNotFound) ||
		errors.Is(err, states.ErrValidation) ||
		errors.As(err, new(*states.StateConflictError)) ||
		errors.As(err, new(*states.CommitWriteError)) {
		return err
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "23505":
			// UNIQUE(project_id, state_hash): the identical transition
			// was already committed — states are content-addressed.
			return states.ErrStateExists
		case pgErr.Code == "23503":
			// foreign_key_violation: a referenced project, branch, state
			// or user does not exist. The branch FK is a domain outcome;
			// every other reference is a validation failure (the uuids
			// were parsed in Go before the transaction).
			if containsBranchFK(pgErr) {
				return states.ErrBranchNotFound
			}
			return states.ErrValidation
		case pgErr.Code == "23514" || pgErr.Code == "23502" || pgErr.Code == "22P02" || isInvalidText(err):
			// check_violation (via), not_null_violation, invalid text
			// representation (malformed uuid/jsonb).
			return states.ErrValidation
		}
	}
	return fmt.Errorf("persistence: commit state: %w", err)
}

// containsBranchFK reports whether the FK violation names the branch
// reference of either table (state insert or commit insert).
func containsBranchFK(pgErr *pgconn.PgError) bool {
	return pgErr.ConstraintName == "project_states_branch_id_fkey" ||
		pgErr.ConstraintName == "state_commits_branch_id_fkey"
}

// optionalUUIDPtr converts a nullable *string id to the pgx uuid type;
// nil (or empty) stays NULL.
func optionalUUIDPtr(s *string) (pgtype.UUID, error) {
	if s == nil || *s == "" {
		return pgtype.UUID{}, nil
	}
	return textUUID(*s)
}

// projectStateFromRow converts a sqlc project_states row to the domain
// value.
func projectStateFromRow(row sqlc.ProjectState) domain.ProjectState {
	return domain.ProjectState{
		ID:              pgUUIDToText(row.ID),
		ProjectID:       pgUUIDToText(row.ProjectID),
		BranchID:        uuidPtr(row.BranchID),
		ParentStateID:   uuidPtr(row.ParentStateID),
		StateHash:       row.StateHash,
		GitCommitSHA:    row.GitCommitSha,
		ManifestVersion: row.ManifestVersion,
		CreatedAt:       row.CreatedAt.Time,
	}
}

// stateCommitFromRow converts a sqlc state_commits row to the domain
// value.
func stateCommitFromRow(row sqlc.StateCommit) domain.StateCommit {
	return domain.StateCommit{
		ID:               pgUUIDToText(row.ID),
		ProjectID:        pgUUIDToText(row.ProjectID),
		BranchID:         pgUUIDToText(row.BranchID),
		BaseStateID:      uuidPtr(row.BaseStateID),
		ResultStateID:    pgUUIDToText(row.ResultStateID),
		ActorID:          pgUUIDToText(row.ActorID),
		Via:              domain.StateVia(row.Via),
		Message:          row.Message,
		OperationSummary: json.RawMessage(row.OperationSummary),
		CreatedAt:        row.CreatedAt.Time,
	}
}
