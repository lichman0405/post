package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/merge"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// SemanticMergeStore is the persistence adapter of the Semantic Merge Engine
// (T0406): it reads the merge scope, takes the row locks the merge's fence
// needs, writes the merge record and the conflicts it carries, and closes the
// PR and the source branch — the writes ON the commit transaction, so the
// accepted state and the record of how it was accepted are one atomic fact.
// The Git saga's outcome is recorded afterwards, on its own narrow surface.
//
// The record's immutability is the database's job, not this adapter's
// (migration 00069): semantic_merges refuses any update outside the Git saga
// columns and treats `updated` as terminal, while semantic_merge_conflicts is
// append-only. An open scientific disagreement that main accepted is a fact
// of that state, never an editable row.
type SemanticMergeStore struct {
	pool *pgxpool.Pool
}

// NewSemanticMergeStore builds the store on pool. The pool may be lazy
// (OpenLazy): the API keeps starting while PostgreSQL is down.
func NewSemanticMergeStore(pool *pgxpool.Pool) *SemanticMergeStore {
	return &SemanticMergeStore{pool: pool}
}

// GetPullRequest implements merge.StorePort: one PR by project and number,
// project-scoped — a PR of another project reports the same not-found outcome
// and never leaks the foreign row's existence (docs/45).
func (s *SemanticMergeStore) GetPullRequest(ctx context.Context, projectID string, number int64) (domain.PullRequest, error) {
	projectUUID, err := textUUID(projectID)
	if err != nil {
		return domain.PullRequest{}, merge.ErrPullRequestNotFound
	}
	row, err := sqlc.New(s.pool).GetPullRequestByProjectAndNumber(ctx, sqlc.GetPullRequestByProjectAndNumberParams{
		ProjectID: projectUUID,
		Number:    number,
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return domain.PullRequest{}, merge.ErrPullRequestNotFound
	}
	if err != nil {
		return domain.PullRequest{}, fmt.Errorf("persistence: get pull request for merge: %w", err)
	}
	return pullRequestFromRow(row), nil
}

// GetBranch implements merge.StorePort: one project-scoped branch, or
// merge.ErrBranchNotFound.
func (s *SemanticMergeStore) GetBranch(ctx context.Context, projectID, branchID string) (domain.Branch, error) {
	projectUUID, err := textUUID(projectID)
	if err != nil {
		return domain.Branch{}, merge.ErrBranchNotFound
	}
	branchUUID, err := textUUID(branchID)
	if err != nil {
		return domain.Branch{}, merge.ErrBranchNotFound
	}
	row, err := sqlc.New(s.pool).GetBranchByProjectAndID(ctx, sqlc.GetBranchByProjectAndIDParams{
		ID:        branchUUID,
		ProjectID: projectUUID,
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return domain.Branch{}, merge.ErrBranchNotFound
	}
	if err != nil {
		return domain.Branch{}, fmt.Errorf("persistence: get branch for merge: %w", err)
	}
	return branchFromRow(row), nil
}

// VersionHeads implements merge.StorePort: the current version counters of
// the containers the plan will append to. This is the OPTIMISTIC prediction
// read — it takes no lock, and the write transaction re-reads the same
// counters under a row lock (LockVersionHeads) and refuses if they moved.
//
// A container with no row is simply absent rather than invented: the service
// reports the missing entity.
func (s *SemanticMergeStore) VersionHeads(ctx context.Context, objectIDs, relationIDs []string) (merge.VersionHeads, error) {
	return readVersionHeads(ctx, s.pool, objectIDs, relationIDs)
}

// LockVersionHeads implements merge.StorePort: VersionHeads on the commit
// transaction, under the row locks the appends take. Locking before writing
// turns a lost counter compare-and-swap into a wait; the CAS stays as the
// backstop.
func (s *SemanticMergeStore) LockVersionHeads(ctx context.Context, tx states.Transaction, objectIDs, relationIDs []string) (merge.VersionHeads, error) {
	return readVersionHeads(ctx, tx, objectIDs, relationIDs)
}

// querier is the read surface the two head reads share: the pool and the
// commit transaction are both pgx query runners.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// readVersionHeads reads the containers' current version counters. Both
// statements carry FOR UPDATE, and that is deliberate: one function serves
// both callers, so the locked read can never drift from the prediction read
// it is compared against.
//
// What the clause buys differs by runner. Called with the commit
// transaction (LockVersionHeads) it is the point: the row locks are held
// for the rest of the transaction and serialize the appends this merge's
// version-counter compare-and-swap then lands. Called with the pool
// (VersionHeads) there is no explicit transaction around the statement, so
// the lock lasts only for the statement itself — the prediction read gets
// no protection from it, and it does not need any: it is optimistic by
// design, and the write transaction's own locked read plus the CAS are what
// actually keep the heads honest.
//
// Raw SQL rather than a generated query because of that clause: the
// generated reads carry no lock by design (the in-transaction appends take
// theirs through the counter compare-and-swap, T0208).
//
// Both lists are walked in ascending id order, so two concurrent merges over
// overlapping containers take their locks in the same order and cannot
// deadlock.
func readVersionHeads(ctx context.Context, q querier, objectIDs, relationIDs []string) (merge.VersionHeads, error) {
	heads := merge.VersionHeads{
		Objects:   make(map[string]int, len(objectIDs)),
		Relations: make(map[string]int, len(relationIDs)),
	}
	ids := append([]string(nil), objectIDs...)
	sort.Strings(ids)
	for _, id := range ids {
		objectUUID, err := textUUID(id)
		if err != nil {
			return merge.VersionHeads{}, merge.ErrValidation
		}
		var current int32
		err = q.QueryRow(ctx, `SELECT current_version_no FROM scientific_objects WHERE id = $1 FOR UPDATE`, objectUUID).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return merge.VersionHeads{}, fmt.Errorf("persistence: read scientific object version head: %w", err)
		}
		heads.Objects[id] = int(current)
	}
	relIDs := append([]string(nil), relationIDs...)
	sort.Strings(relIDs)
	for _, id := range relIDs {
		relationUUID, err := textUUID(id)
		if err != nil {
			return merge.VersionHeads{}, merge.ErrValidation
		}
		var current int32
		err = q.QueryRow(ctx, `SELECT current_version_no FROM relations WHERE id = $1 FOR UPDATE`, relationUUID).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return merge.VersionHeads{}, fmt.Errorf("persistence: read relation version head: %w", err)
		}
		heads.Relations[id] = int(current)
	}
	return heads, nil
}

// LockMergeScope implements merge.StorePort: the merge's rows, re-read ON the
// commit transaction and locked.
//
// This is the fence the T0402 review asked for. The plan was computed from an
// earlier, unlocked read; between that read and this write a concurrent merge
// could have committed. Locking the two branches and the PR here — and letting
// the service compare the locked truth against the planned facts — closes
// that window: a second merge either waits here and then sees the moved head
// (and refuses), or loses the commit's own head compare-and-swap. Both fail
// closed.
//
// The branches are locked in ascending id order so two merges over the same
// pair cannot deadlock.
func (s *SemanticMergeStore) LockMergeScope(ctx context.Context, tx states.Transaction, in merge.LockScopeParams) (merge.MergeScope, error) {
	projectUUID, err := textUUID(in.ProjectID)
	if err != nil {
		return merge.MergeScope{}, merge.ErrValidation
	}
	q := sqlc.New(tx)
	sourceID, targetID := in.SourceBranchID, in.TargetBranchID
	if targetID < sourceID {
		sourceID, targetID = targetID, sourceID
	}
	locked := make(map[string]domain.Branch, 2)
	for _, id := range []string{sourceID, targetID} {
		branchUUID, err := textUUID(id)
		if err != nil {
			return merge.MergeScope{}, merge.ErrBranchNotFound
		}
		row, err := q.GetBranchByProjectAndIDForUpdate(ctx, sqlc.GetBranchByProjectAndIDForUpdateParams{
			ID:        branchUUID,
			ProjectID: projectUUID,
		})
		if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
			return merge.MergeScope{}, merge.ErrBranchNotFound
		}
		if err != nil {
			return merge.MergeScope{}, fmt.Errorf("persistence: lock branch for merge: %w", err)
		}
		locked[id] = branchFromRow(row)
	}
	pr, err := q.GetPullRequestByProjectAndNumberForUpdate(ctx, sqlc.GetPullRequestByProjectAndNumberForUpdateParams{
		ProjectID: projectUUID,
		Number:    in.PRNumber,
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return merge.MergeScope{}, merge.ErrPullRequestNotFound
	}
	if err != nil {
		return merge.MergeScope{}, fmt.Errorf("persistence: lock pull request for merge: %w", err)
	}
	return merge.MergeScope{
		PullRequest: pullRequestFromRow(pr),
		Source:      locked[in.SourceBranchID],
		Target:      locked[in.TargetBranchID],
	}, nil
}

// WriteMerge implements merge.StorePort: the merge row and the conflicts it
// carries, inserted on the commit transaction, so the accepted state and the
// record of how it was accepted cannot come apart.
func (s *SemanticMergeStore) WriteMerge(ctx context.Context, tx states.Transaction, in merge.WriteMergeParams) (domain.SemanticMerge, error) {
	projectUUID, err := textUUID(in.ProjectID)
	if err != nil {
		return domain.SemanticMerge{}, merge.ErrValidation
	}
	// The plan is the engine's canonical JSON. It is decoded to a value and
	// written to the plan jsonb column, which normalizes whitespace and key
	// order: the stored JSON is the same VALUE as the canonical bytes and not
	// the same text. The digest beside it is over the canonical bytes, so an
	// auditor recomputes the plan from the recorded three states and the
	// recorded decisions and hashes THAT — the plan_version + plan_digest pair
	// is the match, never a byte comparison against the column.
	var planJSON map[string]any
	if err := json.Unmarshal(in.PlanJSON, &planJSON); err != nil {
		return domain.SemanticMerge{}, fmt.Errorf("%w: the plan is not a JSON object", merge.ErrValidation)
	}
	prID, err := textUUID(in.PullRequestID)
	if err != nil {
		return domain.SemanticMerge{}, merge.ErrValidation
	}
	sourceID, err := textUUID(in.SourceBranchID)
	if err != nil {
		return domain.SemanticMerge{}, merge.ErrValidation
	}
	targetID, err := textUUID(in.TargetBranchID)
	if err != nil {
		return domain.SemanticMerge{}, merge.ErrValidation
	}
	baseID, err := textUUID(in.BaseStateID)
	if err != nil {
		return domain.SemanticMerge{}, merge.ErrValidation
	}
	sourceStateID, err := textUUID(in.SourceStateID)
	if err != nil {
		return domain.SemanticMerge{}, merge.ErrValidation
	}
	targetStateID, err := textUUID(in.TargetStateID)
	if err != nil {
		return domain.SemanticMerge{}, merge.ErrValidation
	}
	resultStateID, err := textUUID(in.ResultStateID)
	if err != nil {
		return domain.SemanticMerge{}, merge.ErrValidation
	}
	actorID, err := textUUID(in.ActorID)
	if err != nil {
		return domain.SemanticMerge{}, merge.ErrValidation
	}
	gitState := in.GitState
	if !domain.ValidSemanticMergeGitState(gitState) {
		return domain.SemanticMerge{}, fmt.Errorf("%w: %q is not a Git saga state", merge.ErrValidation, gitState)
	}
	var mergeID pgtype.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO semantic_merges (
			project_id, pull_request_id, source_branch_id, target_branch_id,
			base_state_id, source_state_id, target_state_id, result_state_id,
			actor_id, plan_version, plan, plan_digest,
			applied_count, kept_target_count, carried_count, aborted_count, withheld_count,
			git_ref, git_state)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
		RETURNING id`,
		projectUUID, prID, sourceID, targetID,
		baseID, sourceStateID, targetStateID, resultStateID,
		actorID, in.PlanVersion, planJSON, in.PlanDigest,
		in.Applied, in.KeptTarget, in.Carried, in.Aborted, in.Withheld,
		nullText(in.GitRef), string(gitState),
	).Scan(&mergeID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// UNIQUE(pull_request_id): this PR already has a merge. A second
			// merge row would advance the target branch twice.
			return domain.SemanticMerge{}, &merge.AlreadyMergedError{PullRequestID: in.PullRequestID}
		}
		return domain.SemanticMerge{}, fmt.Errorf("persistence: insert semantic merge: %w", err)
	}
	for i := range in.CarriedConflicts {
		if err := insertCarriedConflict(ctx, tx, mergeID, projectUUID, resultStateID, in.CarriedConflicts[i]); err != nil {
			return domain.SemanticMerge{}, err
		}
	}
	if in.IdempotencyKey != nil {
		// The Idempotency-Key ledger entry (migration 00070), on the same
		// transaction as the merge row it points at: a replay can never
		// find an entry whose merge rolled back. UNIQUE(project_id,
		// idempotency_key) makes two concurrent retries with one key a
		// database conflict — the loser's whole transaction (state commit
		// included) rolls back, and the retry then replays the winner.
		if _, err := sqlc.New(tx).CreateMergeCreation(ctx, sqlc.CreateMergeCreationParams{
			ProjectID:      projectUUID,
			IdempotencyKey: *in.IdempotencyKey,
			MergeID:        mergeID,
		}); err != nil {
			return domain.SemanticMerge{}, fmt.Errorf("persistence: record merge creation: %w", err)
		}
	}
	// The audit row (docs/26: governance actions are audited) is part of
	// the same unit. It names the merge id the insert just assigned, which
	// is why the service leaves TargetRef to this adapter.
	if in.Audit.Action != "" {
		audit := in.Audit
		audit.TargetRef = "merge:" + pgUUIDToText(mergeID)
		if err := appendAudit(ctx, sqlc.New(tx), audit); err != nil {
			return domain.SemanticMerge{}, err
		}
	}
	return domain.SemanticMerge{
		ID:             pgUUIDToText(mergeID),
		ProjectID:      in.ProjectID,
		PullRequestID:  in.PullRequestID,
		SourceBranchID: in.SourceBranchID,
		TargetBranchID: in.TargetBranchID,
		BaseStateID:    in.BaseStateID,
		SourceStateID:  in.SourceStateID,
		TargetStateID:  in.TargetStateID,
		ResultStateID:  in.ResultStateID,
		ActorID:        in.ActorID,
		PlanVersion:    in.PlanVersion,
		Plan:           in.PlanJSON,
		PlanDigest:     in.PlanDigest,
		Applied:        in.Applied,
		KeptTarget:     in.KeptTarget,
		Carried:        in.Carried,
		Aborted:        in.Aborted,
		Withheld:       in.Withheld,
		GitRef:         in.GitRef,
		GitState:       gitState,
	}, nil
}

// insertCarriedConflict writes one carried conflict (append-only, migration
// 00069). Both side versions are named, because "carried" means both stay.
func insertCarriedConflict(ctx context.Context, tx states.Transaction, mergeID, projectUUID, resultStateID pgtype.UUID, c merge.CarriedConflict) error {
	targetID, err := textUUID(c.TargetID)
	if err != nil {
		return merge.ErrValidation
	}
	decidedBy, err := textUUID(c.DecidedBy)
	if err != nil {
		// A carried conflict without the human who decided it would record an
		// agent's choice as if it were a decision (CLAUDE.md §9.11).
		return fmt.Errorf("%w: a carried conflict must name the human who decided it", merge.ErrValidation)
	}
	sourceVersionID, err := textUUID(c.SourceVersionID)
	if err != nil {
		return merge.ErrValidation
	}
	var targetVersionID, otherObjectID pgtype.UUID
	if c.TargetVersionID != nil && *c.TargetVersionID != "" {
		if targetVersionID, err = textUUID(*c.TargetVersionID); err != nil {
			return merge.ErrValidation
		}
	}
	if c.OtherObjectID != nil && *c.OtherObjectID != "" {
		if otherObjectID, err = textUUID(*c.OtherObjectID); err != nil {
			return merge.ErrValidation
		}
	}
	fields, err := json.Marshal(c.Fields)
	if err != nil {
		return merge.ErrValidation
	}
	payloadKeys, err := json.Marshal(c.PayloadKeys)
	if err != nil {
		return merge.ErrValidation
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO semantic_merge_conflicts (
			merge_id, project_id, result_state_id, target_kind, target_id,
			conflict_code, conflict_category, conflict_fields, conflict_payload_keys,
			other_object_id, detail, decision, decided_by, note,
			source_version_id, target_version_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		mergeID, projectUUID, resultStateID, string(c.TargetKind), targetID,
		c.Code, c.Category, fields, payloadKeys,
		otherObjectID, c.Detail, string(c.Decision), decidedBy, c.Note,
		sourceVersionID, targetVersionID,
	)
	if err != nil {
		return fmt.Errorf("persistence: insert carried merge conflict: %w", err)
	}
	return nil
}

// MarkPullRequestMerged implements merge.StorePort: merge_ready → merged, on
// the commit transaction. The compare-and-swap leaves any other state
// untouched (migration 00051's guard enforces the same map for every write
// path), so the review machine cannot be skipped by calling the merge engine
// directly.
func (s *SemanticMergeStore) MarkPullRequestMerged(ctx context.Context, tx states.Transaction, projectID, pullRequestID string) error {
	projectUUID, err := textUUID(projectID)
	if err != nil {
		return merge.ErrValidation
	}
	prUUID, err := textUUID(pullRequestID)
	if err != nil {
		return merge.ErrValidation
	}
	tag, err := tx.Exec(ctx, `
		UPDATE pull_requests
		SET state = 'merged', merged_at = now()
		WHERE id = $1 AND project_id = $2 AND state = 'merge_ready'`, prUUID, projectUUID)
	if err != nil {
		return fmt.Errorf("persistence: mark pull request merged: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return &merge.PullRequestNotMergeableError{PullRequestID: pullRequestID}
	}
	return nil
}

// MarkBranchMerged implements merge.StorePort: active → merged, on the commit
// transaction. The guards mirror the branch domain's (migration 00028 and the
// lifecycle transition rule): main's lifecycle is the project's and never
// moves here, and a closed research path stays closed.
func (s *SemanticMergeStore) MarkBranchMerged(ctx context.Context, tx states.Transaction, branchID string) error {
	branchUUID, err := textUUID(branchID)
	if err != nil {
		return merge.ErrValidation
	}
	tag, err := tx.Exec(ctx, `
		UPDATE branches SET lifecycle_state = 'merged'
		WHERE id = $1 AND name <> 'main' AND lifecycle_state = 'active'`, branchUUID)
	if err != nil {
		return fmt.Errorf("persistence: mark source branch merged: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return &merge.BranchNotActiveError{BranchID: branchID}
	}
	return nil
}

// CompleteGitStep implements merge.StorePort: the provider-side merge landed,
// so the row records the sha it produced. `updated` is terminal for the row
// (migration 00069 refuses a move back out of it).
func (s *SemanticMergeStore) CompleteGitStep(ctx context.Context, in merge.GitStepParams) (domain.SemanticMerge, error) {
	mergeUUID, err := textUUID(in.MergeID)
	if err != nil {
		return domain.SemanticMerge{}, merge.ErrValidation
	}
	if in.SHA == "" {
		return domain.SemanticMerge{}, fmt.Errorf("%w: a completed Git step names the merge commit it produced", merge.ErrValidation)
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE semantic_merges
		SET git_state = 'updated', git_sha = $2, git_ref = coalesce($3, git_ref),
		    git_error = '', git_attempts = git_attempts + 1
		WHERE id = $1 AND git_state <> 'updated'`,
		mergeUUID, pgtype.Text{String: in.SHA, Valid: true}, nullText(in.Ref)); err != nil {
		return domain.SemanticMerge{}, fmt.Errorf("persistence: complete merge git step: %w", err)
	}
	return s.rereadMerge(ctx, mergeUUID)
}

// RecordGitAttempt implements merge.StorePort: the Git step did not finish (a
// provider failure, or no adapter wired at all). The row keeps whatever a
// previous attempt recorded — a recorded sha is never cleared — and the step
// stays retryable.
func (s *SemanticMergeStore) RecordGitAttempt(ctx context.Context, in merge.GitStepParams) (domain.SemanticMerge, error) {
	mergeUUID, err := textUUID(in.MergeID)
	if err != nil {
		return domain.SemanticMerge{}, merge.ErrValidation
	}
	switch in.State {
	case domain.GitStatePending, domain.GitStateFailed, domain.GitStateSkipped:
	default:
		// `updated` belongs to CompleteGitStep; anything else is not a saga
		// state (00069's CHECK would refuse it too, but the message here says
		// what the caller got wrong).
		return domain.SemanticMerge{}, fmt.Errorf("%w: %q is not a Git saga retry state", merge.ErrValidation, in.State)
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE semantic_merges
		SET git_state = $2, git_error = $3, git_ref = coalesce($4, git_ref),
		    git_attempts = git_attempts + 1
		WHERE id = $1 AND git_state <> 'updated'`,
		mergeUUID, string(in.State), in.Error, nullText(in.Ref)); err != nil {
		return domain.SemanticMerge{}, fmt.Errorf("persistence: record merge git attempt: %w", err)
	}
	return s.rereadMerge(ctx, mergeUUID)
}

// rereadMerge returns the saga row as stored. It is the tail of both saga
// writes, and it is a read because those writes report no rows in two very
// different cases: the row does not exist, or the saga had already finished
// — `updated` is terminal (migration 00069), so a second completion (and a
// retry after it) updates nothing. A finished saga is not a failure, the
// step is idempotent; the read is what tells the two apart.
func (s *SemanticMergeStore) rereadMerge(ctx context.Context, mergeUUID pgtype.UUID) (domain.SemanticMerge, error) {
	row, err := scanMerge(s.pool.QueryRow(ctx, mergeSelectSQL+` WHERE id = $1`, mergeUUID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.SemanticMerge{}, domain.ErrSemanticMergeNotFound
	}
	if err != nil {
		return domain.SemanticMerge{}, fmt.Errorf("persistence: read merge after git step: %w", err)
	}
	return row, nil
}

// GetMergeByPullRequest implements merge.StorePort: one merge by PR, or
// domain.ErrSemanticMergeNotFound.
func (s *SemanticMergeStore) GetMergeByPullRequest(ctx context.Context, projectID, pullRequestID string) (domain.SemanticMerge, error) {
	projectUUID, err := textUUID(projectID)
	if err != nil {
		return domain.SemanticMerge{}, domain.ErrSemanticMergeNotFound
	}
	prUUID, err := textUUID(pullRequestID)
	if err != nil {
		return domain.SemanticMerge{}, domain.ErrSemanticMergeNotFound
	}
	row, err := scanMerge(s.pool.QueryRow(ctx, mergeSelectSQL+`
		WHERE project_id = $1 AND pull_request_id = $2`, projectUUID, prUUID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.SemanticMerge{}, domain.ErrSemanticMergeNotFound
	}
	if err != nil {
		return domain.SemanticMerge{}, fmt.Errorf("persistence: get semantic merge: %w", err)
	}
	return row, nil
}

// ListPendingGitMerges returns the merges whose Git step has not finished,
// oldest first — the scan a retry needs when the provider was unreachable
// (indexed by migration 00069's partial index).
//
// The product's retry path is the idempotent one (see the merge service's
// replay): a client that repeats the merge request with the same
// Idempotency-Key re-drives the unfinished step. This scan is what a
// background sweeper would use to retry without a client in the loop; it
// is called by this package's integration test rather than by a worker,
// and no worker is wired in this build (justified in T0409's RESULT).
func (s *SemanticMergeStore) ListPendingGitMerges(ctx context.Context, limit int) ([]domain.SemanticMerge, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, mergeSelectSQL+`
		WHERE git_state <> 'updated' ORDER BY created_at, id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("persistence: list pending semantic merges: %w", err)
	}
	defer rows.Close()
	out := make([]domain.SemanticMerge, 0)
	for rows.Next() {
		m, err := scanMerge(rows)
		if err != nil {
			return nil, fmt.Errorf("persistence: scan semantic merge: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// LookupMergeCreation implements merge.StorePort: the ledger read behind the
// Idempotency-Key replay. It returns nil for an unused key (including a
// project id the ledger cannot hold an entry for), and the stored merge row
// for a used one — the same shape release_store.LookupCreation uses, so a
// replay hands the caller the first call's answer instead of a second
// merge's.
func (s *SemanticMergeStore) LookupMergeCreation(ctx context.Context, projectID, idempotencyKey string) (*domain.SemanticMerge, error) {
	if idempotencyKey == "" {
		return nil, nil
	}
	projectUUID, err := textUUID(projectID)
	if err != nil {
		return nil, nil
	}
	mergeID, err := sqlc.New(s.pool).GetMergeCreation(ctx, sqlc.GetMergeCreationParams{
		ProjectID:      projectUUID,
		IdempotencyKey: idempotencyKey,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("persistence: read merge creation: %w", err)
	}
	row, err := scanMerge(s.pool.QueryRow(ctx, mergeSelectSQL+` WHERE id = $1`, mergeID))
	if errors.Is(err, pgx.ErrNoRows) {
		// The ledger and the merge are written on one transaction, so an
		// entry without its row is a store inconsistency — report it as a
		// not-found rather than inventing a merge.
		return nil, domain.ErrSemanticMergeNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("persistence: read merge for creation: %w", err)
	}
	return &row, nil
}

// ListCarriedConflicts implements merge.StorePort: the conflicts one merge
// carried into the accepted state, in insertion order.
func (s *SemanticMergeStore) ListCarriedConflicts(ctx context.Context, mergeID string) ([]domain.SemanticMergeConflict, error) {
	mergeUUID, err := textUUID(mergeID)
	if err != nil {
		return nil, merge.ErrValidation
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, merge_id, project_id, result_state_id, target_kind, target_id,
		       conflict_code, conflict_category, conflict_fields, conflict_payload_keys,
		       other_object_id, detail, decision, decided_by, note,
		       source_version_id, target_version_id, created_at
		FROM semantic_merge_conflicts
		WHERE merge_id = $1
		ORDER BY created_at, id`, mergeUUID)
	if err != nil {
		return nil, fmt.Errorf("persistence: list carried merge conflicts: %w", err)
	}
	defer rows.Close()
	out := make([]domain.SemanticMergeConflict, 0)
	for rows.Next() {
		var (
			id, mid, pid, resultID, targetID                     pgtype.UUID
			otherObject, targetVersion, decidedBy, sourceVersion pgtype.UUID
			fields, payloadKeys                                  []byte
			createdAt                                            pgtype.Timestamptz
			detail, code, category, decision, targetKind, note   string
		)
		if err := rows.Scan(&id, &mid, &pid, &resultID, &targetKind, &targetID,
			&code, &category, &fields, &payloadKeys, &otherObject, &detail, &decision,
			&decidedBy, &note, &sourceVersion, &targetVersion, &createdAt); err != nil {
			return nil, fmt.Errorf("persistence: scan carried merge conflict: %w", err)
		}
		out = append(out, domain.SemanticMergeConflict{
			ID:              pgUUIDToText(id),
			MergeID:         pgUUIDToText(mid),
			ProjectID:       pgUUIDToText(pid),
			ResultStateID:   pgUUIDToText(resultID),
			TargetKind:      domain.ConflictResolutionTargetKind(targetKind),
			TargetID:        pgUUIDToText(targetID),
			Code:            code,
			Category:        category,
			Fields:          decodeStringArray(fields),
			PayloadKeys:     decodeStringArray(payloadKeys),
			OtherObjectID:   uuidPtr(otherObject),
			Detail:          detail,
			Kind:            domain.ResolutionKind(decision),
			DecidedBy:       pgUUIDToText(decidedBy),
			Note:            note,
			SourceVersionID: pgUUIDToText(sourceVersion),
			TargetVersionID: uuidPtr(targetVersion),
			CreatedAt:       createdAt.Time,
		})
	}
	return out, rows.Err()
}

// mergeSelectSQL is the shared column list of the merge reads.
const mergeSelectSQL = `
	SELECT id, project_id, pull_request_id, source_branch_id, target_branch_id,
	       base_state_id, source_state_id, target_state_id, result_state_id, actor_id,
	       plan_version, plan, plan_digest,
	       applied_count, kept_target_count, carried_count, aborted_count, withheld_count,
	       git_ref, git_sha, git_state, git_error, git_attempts, created_at, updated_at
	FROM semantic_merges`

// scanMerge reads one merge row.
func scanMerge(row pgx.Row) (domain.SemanticMerge, error) {
	var (
		m                                                        domain.SemanticMerge
		id, pid, pri, src, tgt, base, srcState, tgtState, result pgtype.UUID
		actor                                                    pgtype.UUID
		planVersion, digest, gitState, gitError                  string
		plan                                                     []byte
		gitRef, gitSHA                                           pgtype.Text
		attempts                                                 int32
		createdAt, updatedAt                                     pgtype.Timestamptz
	)
	if err := row.Scan(&id, &pid, &pri, &src, &tgt, &base, &srcState, &tgtState, &result, &actor,
		&planVersion, &plan, &digest,
		&m.Applied, &m.KeptTarget, &m.Carried, &m.Aborted, &m.Withheld,
		&gitRef, &gitSHA, &gitState, &gitError, &attempts, &createdAt, &updatedAt); err != nil {
		return domain.SemanticMerge{}, err
	}
	m.ID = pgUUIDToText(id)
	m.ProjectID = pgUUIDToText(pid)
	m.PullRequestID = pgUUIDToText(pri)
	m.SourceBranchID = pgUUIDToText(src)
	m.TargetBranchID = pgUUIDToText(tgt)
	m.BaseStateID = pgUUIDToText(base)
	m.SourceStateID = pgUUIDToText(srcState)
	m.TargetStateID = pgUUIDToText(tgtState)
	m.ResultStateID = pgUUIDToText(result)
	m.ActorID = pgUUIDToText(actor)
	m.PlanVersion = planVersion
	m.Plan = plan
	m.PlanDigest = digest
	m.GitRef = textPtr(gitRef)
	m.GitSHA = textPtr(gitSHA)
	m.GitState = domain.SemanticMergeGitState(gitState)
	m.GitError = gitError
	m.GitAttempts = int(attempts)
	m.CreatedAt = createdAt.Time
	m.UpdatedAt = updatedAt.Time
	return m, nil
}

// nullText renders an optional string as a NULL-able text parameter.
func nullText(s *string) pgtype.Text {
	if s == nil || *s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

// textPtr reads an optional text column back.
func textPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

// decodeStringArray decodes a jsonb string array column.
func decodeStringArray(raw []byte) []string {
	if len(raw) == 0 {
		return []string{}
	}
	out := []string{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return []string{}
	}
	return out
}

// The adapter's contract is checked at compile time: the merge service's
// store port is exactly what this store implements.
var _ merge.StorePort = (*SemanticMergeStore)(nil)
