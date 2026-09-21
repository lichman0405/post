package persistence

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

// ProposesToProject implements diffs.ProposalPort (T0817): whether a pull
// request of projectID proposes stateID as its head. It is the read the
// three-way diff makes about the ONE side of its triple that may live in
// another project — the external fork's source state (docs/04 §2).
//
// The question is deliberately about the proposal and not about fork
// lineage: what the diff needs is a bound on what a foreign source state
// may expose, and "this project's own pull request proposes it" is
// exactly the set of foreign states the project is already shown. A state
// no proposal names — including one of a fork this project never saw a
// proposal from — answers false, and the diff then refuses it as the
// foreign state it always refused.
//
// Every id that cannot name a row answers false rather than failing: the
// caller is asking a closed question about stored content, and "no such
// proposal" is the answer that keeps the diff's membership check intact.
func (s *PullRequestStore) ProposesToProject(ctx context.Context, projectID, stateID string) (bool, error) {
	projectUUID, err := textUUID(projectID)
	if err != nil {
		return false, nil
	}
	stateUUID, err := textUUID(stateID)
	if err != nil {
		return false, nil
	}
	var proposed bool
	err = s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pull_requests WHERE project_id = $1 AND proposed_state_id = $2)`,
		projectUUID, stateUUID).Scan(&proposed)
	if err != nil {
		return false, fmt.Errorf("persistence: read proposal of state: %w", err)
	}
	return proposed, nil
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
			errors.Is(err, pullrequests.ErrBranchUnstructuredChanges) ||
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

// GetPullRequestByCreationKey implements pullrequests.Repository: the
// creation replay read (migration 00089's per-project unique key). An
// unknown or foreign project, an unused key and the empty key all report
// ErrPullRequestNotFound — the query itself excludes ” so that the
// "no key" rows a key-less creation writes can never be reached, and the
// project scoping means a foreign project's key answers exactly as an
// unused one does (docs/45: never leak another project's entity
// existence).
func (s *PullRequestStore) GetPullRequestByCreationKey(ctx context.Context, projectID, creationKey string) (domain.PullRequest, error) {
	pid, err := textUUID(projectID)
	if err != nil {
		return domain.PullRequest{}, pullrequests.ErrPullRequestNotFound
	}
	row, err := sqlc.New(s.pool).GetPullRequestByCreationKey(ctx, sqlc.GetPullRequestByCreationKeyParams{
		ProjectID:   pid,
		CreationKey: creationKey,
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return domain.PullRequest{}, pullrequests.ErrPullRequestNotFound
	}
	if err != nil {
		return domain.PullRequest{}, fmt.Errorf("persistence: get pull request by creation key: %w", err)
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

// reviewEntryStates are the two states docs/43 lets a proposal be sent to
// review FROM, in the order this adapter tries them: `open` is the first
// submission, `changes_requested` is the review loop's re-entry after the
// author answered the requested changes. Migration 00051's transition map
// spells both edges (00051:88 and 00051:90) and is the backstop for this
// list.
var reviewEntryStates = []domain.PullRequestState{
	domain.PullRequestStateOpen,
	domain.PullRequestStateChangesRequested,
}

// RequestReview implements pullrequests.Repository: the review-request
// move (docs/43 open | changes_requested -> review_required), one audit
// row, one transaction.
//
// # Why there is no idempotency table here
//
// The state IS the idempotency record, and that is a constraint rather
// than a preference: this task may not add a migration (its scope
// excludes infra/migrations/**), so the ledger the merge route keeps
// (merge_creations, 00070) is not available to it. The tree already
// answers this exact situation for the freeze (internal/application/
// mainfreeze/doc.go, "Idempotency is the state, not a ledger"): the
// Idempotency-Key is required on the route and CONSUMED by the state. A
// repeated request — the same key or a different one — finds the PR
// already in review_required and is answered with the row, writing
// nothing: one audit row forever, by construction. The key itself is
// recorded on the audit row (the request that asked), never consulted.
//
// # The shape of the move
//
// A conditional update, not a read-then-write: the compare-and-swap's
// WHERE clause names the state it is willing to move FROM, so of two
// concurrent requests exactly one can match and only the winner writes
// the audit row. The two entry states are tried as two CAS attempts
// rather than one `IN (...)` predicate, and that is deliberate — it is
// how the winner knows WHICH edge it travelled (the audit row's
// before_summary reports it, and "the first submission" and "re-entered
// review after changes" are different facts to the Activity page). The
// attempts cannot both match: the first one that lands leaves the row in
// review_required, which is in neither WHERE clause.
//
// The loser's CAS matches nothing, and the classify read below then says
// why: the PR is already in review (the replay — a success, not a
// conflict), or it is in a state this edge does not leave from. That read
// runs inside the same transaction and takes the row lock, so the reason
// it reports is the state the request actually met.
//
// # Why the refusal does not single out the terminal states
//
// A merged/closed/aborted PR is refused as *TransitionError, the same
// outcome approved and merge_ready get, and NOT as *TerminalError — which
// this package raises for RefreshProposedState and the review-submission
// path raises for a PR a review can no longer move. Two reasons, and the
// first is the binding one:
//
//   - It is the behaviour this command has had since T0402, when it was
//     the setState family's move (Close/Abort/MarkMerged/SetState all
//     answer *TransitionError for a source state docs/43's map refuses,
//     terminal ones included). An existing acceptance test pins it
//     (tests/integration/pullrequest_test.go, "closed -> review_required
//     ... want TransitionError"), and nothing about this task asks for a
//     finer split: the contract declares ONE 409 for the operation
//     ("The PR is not in a state that can be sent to review",
//     specs/api/openapi.yaml) and both answers land on it.
//   - A terminal PR is still refused, and refused by the map rather than
//     by a special case: docs/43 gives merged/closed/aborted no outgoing
//     edge, so "not a state that can be sent to review" is exactly what
//     is true of it.
func (s *PullRequestStore) RequestReview(ctx context.Context, projectID string, number int64, idempotencyKey string) (domain.PullRequest, error) {
	pid, err := textUUID(projectID)
	if err != nil {
		return domain.PullRequest{}, pullrequests.ErrPullRequestNotFound
	}
	var out domain.PullRequest
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		for _, expected := range reviewEntryStates {
			row, casErr := q.SetPullRequestState(ctx, sqlc.SetPullRequestStateParams{
				ProjectID: pid,
				Number:    number,
				Expected:  string(expected),
				NextState: string(domain.PullRequestStateReviewRequired),
			})
			if casErr == nil {
				out = pullRequestFromRow(row)
				return appendAudit(ctx, q, domain.AuditEntry{
					Action:    domain.ActionPullRequestReviewRequested,
					ProjectID: projectID,
					TargetRef: "pull_request:" + out.ID,
					BeforeSummary: map[string]any{
						"state": string(expected),
					},
					AfterSummary: map[string]any{
						"state":               string(domain.PullRequestStateReviewRequired),
						"pull_request_id":     out.ID,
						"pull_request_number": out.Number,
						"source_branch_id":    out.SourceBranchID,
						"target_branch_id":    out.TargetBranchID,
					},
					Metadata: map[string]any{
						// The request that asked, recorded and never
						// consulted (see the method doc): an operator
						// tracing a retry can find it, and no code path
						// reads it back.
						"idempotency_key": idempotencyKey,
					},
				})
			}
			if !errors.Is(casErr, pgx.ErrNoRows) {
				if isInvalidText(casErr) {
					return pullrequests.ErrPullRequestNotFound
				}
				return fmt.Errorf("persistence: request review: %w", casErr)
			}
		}
		// Nothing moved. Read the row the request actually met — locked,
		// inside this transaction — and answer for the state it is in.
		current, err := q.GetPullRequestByProjectAndNumberForUpdate(ctx, sqlc.GetPullRequestByProjectAndNumberForUpdateParams{
			ProjectID: pid,
			Number:    number,
		})
		if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
			return pullrequests.ErrPullRequestNotFound
		}
		if err != nil {
			return fmt.Errorf("persistence: request review: %w", err)
		}
		out = pullRequestFromRow(current)
		state := domain.PullRequestState(current.State)
		if state == domain.PullRequestStateReviewRequired {
			// The state the request asked for is the state the PR is in.
			// Nothing is written and the row comes back, which is what
			// makes a repeat answer the same way instead of failing the
			// transition map.
			return nil
		}
		// One refusal for every state docs/43 gives this edge no way out
		// of — approved, merge_ready, and the three terminal states alike
		// (see the method doc: the command's own behaviour since T0402,
		// and the contract's single 409).
		return &pullrequests.TransitionError{
			Number: number,
			From:   state,
			To:     domain.PullRequestStateReviewRequired,
		}
	})
	if err != nil {
		return domain.PullRequest{}, err
	}
	return out, nil
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

// The two database gates a pull_requests INSERT runs into, told apart by
// the text their RAISE statement starts with. Both raise SQLSTATE P0001
// (infra/migrations/00042 and 00086 use the same ERRCODE), so the code
// alone cannot say which rule refused the row — and the two outcomes are
// not interchangeable: one is the caller's own content (409, they can fix
// it) and the other is a cross-project source the application layer
// refuses as a permission outcome before the insert is ever attempted.
//
// The prefixes are the migrations' own sentences, one line each. Matching
// on them is prefix matching on purpose: the rest of each message names
// row identifiers that must not be parsed. A message that matches NEITHER
// — a third trigger added later, or a rewording that leaves these stale —
// stays a store failure and is answered 503, which is the fail-closed
// direction: an unplaceable refusal is never reported as a rule the caller
// can act on.
const (
	// semanticGateRaise is 00042's pull_request_semantic_gate: the source
	// branch's recorded semantic state is unstructured_changes, so no
	// formal PR may be opened from it (docs/16 §4).
	semanticGateRaise = "pull request cannot be opened from branch "
	// forkGateRaise is 00086's pull_request_fork_gate: a cross-project
	// source branch that is not the opener's own fork of the PR's project.
	// That read is the store's own check as well (it answers
	// ErrBranchNotFound for the same condition before the insert); this is
	// the backstop for a path that got past it.
	forkGateRaise = "pull request on project "
)

// mapPullRequestWriteError turns a failed PR insert into the package
// outcomes: foreign-key and constraint violations on the validated input
// are domain validation results, a refusal by one of the two semantic
// gates is that gate's own outcome, and everything else is an adapter
// failure with the cause kept.
func mapPullRequestWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "23503" || pgErr.Code == "23502" || pgErr.Code == "22P02":
			return pullrequests.ErrValidation
		case pgErr.Code == "P0001" && strings.HasPrefix(pgErr.Message, semanticGateRaise):
			// Both verbs are %w: the outcome is the gate's, and the
			// database's own refusal stays in the chain. Its text names the
			// branch and the semantic state it refused on — what an operator
			// reading a log needs, and what a caller that asserts on the
			// gate itself (tests/integration/external_fork_e2e_test.go) reads
			// back. Dropping it here would answer "unstructured changes" to a
			// caller with no way to see which rule said so.
			return fmt.Errorf("%w: %w", pullrequests.ErrBranchUnstructuredChanges, err)
		case pgErr.Code == "P0001" && strings.HasPrefix(pgErr.Message, forkGateRaise) &&
			strings.Contains(pgErr.Message, "cannot take its source branch from project"):
			return pullrequests.ErrBranchNotFound
		}
	}
	if isInvalidText(err) {
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
