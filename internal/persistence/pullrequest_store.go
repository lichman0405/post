package persistence

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// PullRequestStore is the production pullrequests.Repository adapter over
// PostgreSQL (sqlc generated queries, pgx).
//
// Creation invariants run in ONE transaction: the project row is read and
// locked (existence, and the lock that serializes per-project number
// allocation — MAX(number)+1 can never race two concurrent creations of
// the same project), the branch pair is read project-scoped (existence,
// active lifecycle, heads), and the row is inserted with the base state
// pinned from the target branch's head and the proposed state from the
// source branch's head — never from caller-supplied copies.
//
// The base state is fixed for the row's lifetime: there is no update
// path here, and the database rejects any other path (migration 00051).
// The proposed state moves only through RefreshProposedState, the one
// method that sets the transaction-scoped flag the fixity guard
// requires. State transitions are compare-and-swaps; the same docs/43
// map is additionally enforced by the database trigger for ANY update
// path, so the machine cannot be skipped even against code that bypasses
// this adapter (the same discipline as 00028's branch lifecycle guard).
type PullRequestStore struct {
	pool *pgxpool.Pool
}

// NewPullRequestStore builds the store on pool. The pool may be lazy
// (OpenLazy): the API keeps starting while PostgreSQL is down.
func NewPullRequestStore(pool *pgxpool.Pool) *PullRequestStore {
	return &PullRequestStore{pool: pool}
}

// CreatePullRequest implements pullrequests.Repository.
func (s *PullRequestStore) CreatePullRequest(ctx context.Context, in pullrequests.CreatePullRequestParams) (domain.PullRequest, error) {
	projectID, err := textUUID(in.ProjectID)
	if err != nil {
		return domain.PullRequest{}, pullrequests.ErrValidation
	}
	sourceBranchID, err := textUUID(in.SourceBranchID)
	if err != nil {
		return domain.PullRequest{}, pullrequests.ErrValidation
	}
	targetBranchID, err := textUUID(in.TargetBranchID)
	if err != nil {
		return domain.PullRequest{}, pullrequests.ErrValidation
	}
	createdBy, err := textUUID(in.CreatedBy)
	if err != nil {
		return domain.PullRequest{}, pullrequests.ErrValidation
	}
	// Shape backstop (the service is the normal path): the branch pair
	// must differ and the text fields must fit the domain bounds.
	if in.SourceBranchID == in.TargetBranchID ||
		!domain.ValidPullRequestTitle(in.Title) ||
		!domain.ValidPullRequestBody(in.Body) {
		return domain.PullRequest{}, pullrequests.ErrValidation
	}
	var created domain.PullRequest
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		// The project read doubles as the number-allocation lock: the
		// row lock serializes concurrent creations, so MAX(number)+1
		// below never races (task requirement "number per project").
		if _, err := q.GetProjectByIDForUpdate(ctx, projectID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return projects.ErrProjectNotFound
			}
			return err
		}
		// The creation replay (T0410, migration 00089): a request that
		// repeats an Idempotency-Key gets the proposal the first request
		// opened, and nothing is written a second time. The read is INSIDE
		// this transaction and after the project row lock, so two
		// concurrent repeats of one key cannot both miss it: the second
		// waits on the lock, then reads the row the first committed. An
		// empty key names nothing (it is the shared "no key" value) and is
		// answered by the query's own `creation_key <> ''`, not by a
		// check here that a later edit could drop.
		if in.CreationKey != "" {
			replayed, err := q.GetPullRequestByCreationKey(ctx, sqlc.GetPullRequestByCreationKeyParams{
				ProjectID:   projectID,
				CreationKey: in.CreationKey,
			})
			switch {
			case err == nil:
				created = pullRequestFromRow(replayed)
				return nil
			case errors.Is(err, pgx.ErrNoRows), isInvalidText(err):
				// No proposal carries this key yet: open one.
			default:
				return err
			}
		}
		// The source branch is read WITHOUT the project scope: an external
		// proposal's source branch lives in the contributor's own fork
		// project (docs/04 §2), which is a different project from the one
		// the PR proposes to. What keeps that honest is the check right
		// after the read — and 00086's pull_request_fork_gate, which
		// enforces the same rule for ANY insert path, so a store that
		// forgot the check could not open the PR either.
		var sourceID, sourceProject, sourceBase pgtype.UUID
		var sourceLifecycle string
		err = tx.QueryRow(ctx,
			`SELECT id, project_id, base_state_id, lifecycle_state FROM branches WHERE id = $1`,
			sourceBranchID).Scan(&sourceID, &sourceProject, &sourceBase, &sourceLifecycle)
		if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
			return pullrequests.ErrBranchNotFound
		}
		if err != nil {
			return err
		}
		if sourceProject != projectID {
			// A cross-project source is the external fork's shape: it is
			// allowed exactly when the source branch's project is a fork
			// of THIS project forked by THIS PR's creator
			// (open_pr = allow_from_fork for a non-member). Any other
			// foreign source — an unrelated project, or somebody else's
			// fork — reports the very same not-found outcome as a branch
			// that does not exist (docs/45: a foreign entity's existence
			// is never confirmed to a caller who may not use it). Naming
			// the refusal differently here would say "that branch exists,
			// in another project", which is the leak this read is
			// unscoped to avoid, so the identity of the answer must not
			// change with the reason. 00086's pull_request_fork_gate
			// backstops the rule for any other insert path.
			var ownFork bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM project_forks
				   WHERE fork_project_id = $1 AND parent_project_id = $2 AND forked_by = $3)`,
				sourceProject, projectID, createdBy).Scan(&ownFork); err != nil {
				return err
			}
			if !ownFork {
				return pullrequests.ErrBranchNotFound
			}
		}
		target, err := q.GetBranchByProjectAndID(ctx, sqlc.GetBranchByProjectAndIDParams{
			ID:        targetBranchID,
			ProjectID: projectID,
		})
		if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
			return pullrequests.ErrBranchNotFound
		}
		if err != nil {
			return err
		}
		// A proposal never starts from or targets a closed research path
		// (docs/43: merged/aborted history immutable).
		if sourceLifecycle != string(domain.BranchLifecycleActive) {
			return &pullrequests.BranchNotActiveError{
				BranchID:  pgUUIDToText(sourceID),
				Lifecycle: sourceLifecycle,
			}
		}
		if target.LifecycleState != string(domain.BranchLifecycleActive) {
			return &pullrequests.BranchNotActiveError{
				BranchID:  pgUUIDToText(target.ID),
				Lifecycle: target.LifecycleState,
			}
		}
		if !sourceBase.Valid || !target.BaseStateID.Valid {
			return pullrequests.ErrBranchHeadMissing
		}
		// Number allocation: project row locked above, so MAX+1 is
		// race-free; the first PR of a project is #1. (Raw SQL here
		// because sqlc's expression inference mis-types MAX arithmetic;
		// the same discipline as profile_store's membership SQL.)
		var number int64
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(MAX(number), 0) + 1 FROM pull_requests WHERE project_id = $1`,
			projectID).Scan(&number); err != nil {
			return err
		}
		row, err := q.CreatePullRequest(ctx, sqlc.CreatePullRequestParams{
			ProjectID:       projectID,
			Number:          number,
			SourceBranchID:  sourceBranchID,
			TargetBranchID:  targetBranchID,
			BaseStateID:     target.BaseStateID,
			ProposedStateID: sourceBase,
			Title:           in.Title,
			Body:            in.Body,
			CreatedBy:       createdBy,
			CreationKey:     in.CreationKey,
		})
		if err != nil {
			return mapPullRequestWriteError(err)
		}
		created = pullRequestFromRow(row)
		return nil
	})
	if err != nil {
		if errors.Is(err, pullrequests.ErrBranchNotFound) ||
			errors.Is(err, pullrequests.ErrBranchNotActive) ||
			errors.Is(err, pullrequests.ErrBranchHeadMissing) ||
			errors.Is(err, pullrequests.ErrValidation) ||
			errors.Is(err, projects.ErrProjectNotFound) ||
			errors.As(err, new(*pullrequests.BranchNotActiveError)) {
			return domain.PullRequest{}, err
		}
		return domain.PullRequest{}, fmt.Errorf("persistence: create pull request: %w", err)
	}
	return created, nil
}

// GetPullRequest implements pullrequests.Repository.
func (s *PullRequestStore) GetPullRequest(ctx context.Context, projectID string, number int64) (domain.PullRequest, error) {
	pid, err := textUUID(projectID)
	if err != nil {
		return domain.PullRequest{}, pullrequests.ErrPullRequestNotFound
	}
	row, err := sqlc.New(s.pool).GetPullRequestByProjectAndNumber(ctx, sqlc.GetPullRequestByProjectAndNumberParams{
		ProjectID: pid,
		Number:    number,
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return domain.PullRequest{}, pullrequests.ErrPullRequestNotFound
	}
	if err != nil {
		return domain.PullRequest{}, fmt.Errorf("persistence: get pull request: %w", err)
	}
	return pullRequestFromRow(row), nil
}

// ListPullRequests implements pullrequests.Repository. An unknown project
// has no PRs: empty list, not an error (the same read discipline as the
// version logs).
func (s *PullRequestStore) ListPullRequests(ctx context.Context, projectID string) ([]domain.PullRequest, error) {
	id, err := textUUID(projectID)
	if err != nil {
		return nil, nil // cannot name a project; empty, not an error
	}
	rows, err := sqlc.New(s.pool).ListPullRequestsByProject(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("persistence: list pull requests: %w", err)
	}
	out := make([]domain.PullRequest, 0, len(rows))
	for _, row := range rows {
		out = append(out, pullRequestFromRow(row))
	}
	return out, nil
}

// SetPullRequestState implements pullrequests.Repository. The transition
// is a compare-and-swap on the expected state; the zero-row outcome is
// distinguished by one read: not in the project (never leak a foreign
// entity) or moved underneath (concurrent transition).
func (s *PullRequestStore) SetPullRequestState(ctx context.Context, projectID string, number int64, expected, to domain.PullRequestState) (domain.PullRequest, error) {
	if !domain.ValidPullRequestState(expected) || !domain.ValidPullRequestState(to) || to == domain.PullRequestStateOpen {
		return domain.PullRequest{}, pullrequests.ErrValidation
	}
	pid, err := textUUID(projectID)
	if err != nil {
		return domain.PullRequest{}, pullrequests.ErrPullRequestNotFound
	}
	row, err := sqlc.New(s.pool).SetPullRequestState(ctx, sqlc.SetPullRequestStateParams{
		ProjectID: pid,
		Number:    number,
		Expected:  string(expected),
		NextState: string(to),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		current, gerr := s.GetPullRequest(ctx, projectID, number)
		if errors.Is(gerr, pullrequests.ErrPullRequestNotFound) {
			return domain.PullRequest{}, pullrequests.ErrPullRequestNotFound
		}
		if gerr != nil {
			return domain.PullRequest{}, gerr
		}
		return domain.PullRequest{}, &pullrequests.StateConflictError{
			Number:  number,
			Current: current.State,
		}
	}
	if err != nil {
		if isInvalidText(err) {
			return domain.PullRequest{}, pullrequests.ErrPullRequestNotFound
		}
		return domain.PullRequest{}, fmt.Errorf("persistence: set pull request state: %w", err)
	}
	return pullRequestFromRow(row), nil
}

// RefreshProposedState implements pullrequests.Repository — the explicit
// head refresh, the ONLY path that moves the proposed state. One
// transaction: the PR row is locked (serializing against concurrent
// transitions), the terminal check runs on the locked row, the source
// branch's current head is read, the transaction-scoped flag migration
// 00051's fixity guard requires is set, and the row is re-pinned.
func (s *PullRequestStore) RefreshProposedState(ctx context.Context, projectID string, number int64) (domain.PullRequest, error) {
	pid, err := textUUID(projectID)
	if err != nil {
		return domain.PullRequest{}, pullrequests.ErrPullRequestNotFound
	}
	var refreshed domain.PullRequest
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		row, err := q.GetPullRequestByProjectAndNumberForUpdate(ctx, sqlc.GetPullRequestByProjectAndNumberForUpdateParams{
			ProjectID: pid,
			Number:    number,
		})
		if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
			return pullrequests.ErrPullRequestNotFound
		}
		if err != nil {
			return err
		}
		if domain.IsTerminalPullRequestState(domain.PullRequestState(row.State)) {
			return &pullrequests.TerminalError{Number: number, State: domain.PullRequestState(row.State)}
		}
		source, err := q.GetBranchByID(ctx, row.SourceBranchID)
		if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
			// The FK would prevent this; defensive, same outcome as a
			// headless branch below.
			return pullrequests.ErrBranchNotFound
		}
		if err != nil {
			return err
		}
		if !source.BaseStateID.Valid {
			return pullrequests.ErrBranchHeadMissing
		}
		// The explicit-refresh flag the fixity guard requires; is_local
		// means the value dies with the transaction.
		if err := q.EnablePullRequestHeadRefresh(ctx); err != nil {
			return err
		}
		updated, err := q.RefreshPullRequestProposedState(ctx, sqlc.RefreshPullRequestProposedStateParams{
			ProjectID:       pid,
			Number:          number,
			ProposedStateID: source.BaseStateID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return pullrequests.ErrPullRequestNotFound
		}
		if err != nil {
			return err
		}
		refreshed = pullRequestFromRow(updated)
		return nil
	})
	if err != nil {
		if errors.Is(err, pullrequests.ErrPullRequestNotFound) ||
			errors.Is(err, pullrequests.ErrBranchNotFound) ||
			errors.Is(err, pullrequests.ErrBranchHeadMissing) ||
			errors.As(err, new(*pullrequests.TerminalError)) {
			return domain.PullRequest{}, err
		}
		return domain.PullRequest{}, fmt.Errorf("persistence: refresh pull request head: %w", err)
	}
	return refreshed, nil
}

// mapPullRequestWriteError turns a failed PR insert into the package
// outcomes: foreign-key and constraint violations on the validated input
// are domain validation results, everything else is an adapter failure
// with the cause kept.
func mapPullRequestWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "23503" || pgErr.Code == "23502" || pgErr.Code == "22P02") || isInvalidText(err) {
		return pullrequests.ErrValidation
	}
	return err
}

// pullRequestFromRow converts a sqlc pull_requests row to the domain
// value.
func pullRequestFromRow(row sqlc.PullRequest) domain.PullRequest {
	return domain.PullRequest{
		ID:              pgUUIDToText(row.ID),
		ProjectID:       pgUUIDToText(row.ProjectID),
		Number:          row.Number,
		SourceBranchID:  pgUUIDToText(row.SourceBranchID),
		TargetBranchID:  pgUUIDToText(row.TargetBranchID),
		BaseStateID:     pgUUIDToText(row.BaseStateID),
		ProposedStateID: pgUUIDToText(row.ProposedStateID),
		Title:           row.Title,
		Body:            row.Body,
		State:           domain.PullRequestState(row.State),
		CreatedBy:       pgUUIDToText(row.CreatedBy),
		CreatedAt:       row.CreatedAt.Time,
		MergedAt:        timestamptzPtr(row.MergedAt),
	}
}
