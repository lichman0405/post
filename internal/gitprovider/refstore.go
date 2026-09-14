package gitprovider

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The canonical-store side of branch-ref sync (T0303): the only reader and
// writer of git_branch_refs.sync_state / fork_sha / head_sha. This package
// owns those columns by task assignment (like git_repository_provisions in
// T0301); the mapping rows themselves are created by migration 00031's
// triggers, so every branches row has exactly one — whatever path inserted
// the branch.
//
// head_sha is the one column other product paths read: the ref's last
// known tip (T0305 keeps it fresh on push ingestion, T0406's merge engine
// reads it).

// ErrBranchNotFound: the branch row does not exist — or rather, its
// backlog row cannot be derived (no branch, no mapping row, or a project
// that is not provisioned yet: indistinguishable to the caller, and all
// three mean "nothing to sync right now").
var ErrBranchNotFound = errors.New("gitprovider: branch ref not found")

// PGBranchRefStore is the PostgreSQL adapter for the git_branch_refs table
// (plain pgx, same discipline as PGProvisionStore: the columns belong to
// this package, sqlc queries would put them on the shared persistence
// surface). It implements BranchRefStore.
type PGBranchRefStore struct {
	pool *pgxpool.Pool
}

// NewBranchRefStore builds the store on pool. The pool may be lazy
// (persistence.OpenLazy): the API keeps starting while PostgreSQL is down.
func NewBranchRefStore(pool *pgxpool.Pool) *PGBranchRefStore {
	return &PGBranchRefStore{pool: pool}
}

// backlogSQL derives one work item per backlog row. The join on
// git_repository_provisions is the gate: a branch whose project is not
// provisioned yet has no repository to sync into, so it never enters the
// backlog — and keeps its current state, so the next sweep sees it once
// provisioning lands. fork_sha prefers the base state's own recorded
// commit; the NULL fallback (default branch) is the syncer's decision.
const backlogSQL = `
	SELECT r.branch_id, r.git_ref, b.name, p.owner, p.name,
	       COALESCE(s.git_commit_sha, ''),
	       r.close_requested_at IS NOT NULL
	  FROM git_branch_refs r
	  JOIN branches b ON b.id = r.branch_id
	  JOIN projects pr ON pr.id = b.project_id
	  JOIN git_repository_provisions p ON p.project_id = pr.id
	  LEFT JOIN project_states s ON s.id = b.base_state_id
	 WHERE r.sync_state IN ('pending','failed','closing')`

// backlogOrder is the shared backlog ordering (oldest first); the
// single-row lock query inserts its branch filter BEFORE it — appending
// after the ORDER BY is not SQL.
const backlogOrder = ` ORDER BY r.created_at, r.branch_id`

// BranchRefBacklog lists every branch awaiting sync, oldest first: pending
// (never attempted), failed (the last attempt failed) and closing (the
// semantic branch is terminal, the ref must be deleted). The boot sweep
// re-enqueues the whole backlog on every API start — that is the bounded
// retry policy: a transient provider outage marks rows failed, and the
// next sweep (or a redelivered job) re-attempts them, with no hot retry
// loop.
func (s *PGBranchRefStore) BranchRefBacklog(ctx context.Context) ([]PendingBranchRef, error) {
	rows, err := s.pool.Query(ctx, backlogSQL+backlogOrder)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingBranchRef
	for rows.Next() {
		var p PendingBranchRef
		if err := rows.Scan(&p.BranchID, &p.GitRef, &p.Name, &p.Owner, &p.Repo, &p.ForkSHA, &p.Close); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SyncBranchRef serializes one sync attempt per branch: the mapping row is
// locked FOR UPDATE, a row that already reached a terminal state ('closed')
// — or that left the backlog between the sweep and the lock (a concurrent
// sync won the race) — is skipped (idempotency — a redelivered job is a
// no-op), otherwise fn performs the provider work while the lock is held
// and its outcome is committed in the same transaction. fn must be quick
// (the lock is held across it; the job timeout bounds it) and must not use
// this store itself.
//
// On success the row moves to 'synced' (create direction: fork point and
// head recorded) or 'closed' (close direction: the final head recorded,
// the ref deleted). On failure the row moves to 'failed' — the canonical
// store records the state, the job layer's structured logs carry the
// redacted reason, and the error is returned to the loop for retry. A
// failed row stays in the backlog, so the next boot sweep or redelivered
// job re-attempts it: a transient provider outage never wedges a branch.
// Rows whose project is not provisioned yet skip silently — provisioning
// (T0301) gates the backlog join itself, so they are only reached through
// a job that raced the provision.
func (s *PGBranchRefStore) SyncBranchRef(ctx context.Context, branchID string, fn func(PendingBranchRef) (*BranchRefRecord, error)) (synced, skipped bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var p PendingBranchRef
	err = tx.QueryRow(ctx, backlogSQL+` AND r.branch_id = $1`+backlogOrder+` FOR UPDATE OF r`, branchID).
		Scan(&p.BranchID, &p.GitRef, &p.Name, &p.Owner, &p.Repo, &p.ForkSHA, &p.Close)
	if errors.Is(err, pgx.ErrNoRows) {
		// No backlog row under the lock: the branch does not exist, its
		// project is not provisioned, or the row already moved on
		// (including 'closed' — the backlog filter excludes it, and a
		// concurrent sync that won the race between sweep and lock) —
		// nothing to do now.
		return false, true, nil
	}
	if err != nil {
		return false, false, err
	}

	rec, fnErr := fn(p)
	if fnErr != nil {
		// The canonical failure record is the state transition; the reason
		// travels on the returned error (the job loop logs it with the
		// job's correlation id — the provider errors are built redacted by
		// construction, never carrying the token or the configured URLs).
		// The guard trigger (00031) allows failed→failed: a retry that
		// fails again re-records the same state.
		if _, uerr := tx.Exec(ctx,
			`UPDATE git_branch_refs SET sync_state = 'failed', updated_at = now() WHERE branch_id = $1`,
			branchID); uerr != nil {
			if cerr := tx.Commit(ctx); cerr != nil {
				return false, false, fmt.Errorf("%w (and recording the failure failed: %v)", fnErr, cerr)
			}
			return false, false, fmt.Errorf("%w (recording the failure failed: %v)", fnErr, uerr)
		}
		if err := tx.Commit(ctx); err != nil {
			return false, false, err
		}
		return false, false, fnErr
	}
	if rec == nil {
		// fn succeeded but returned no record — a programming error on the
		// caller's side, never a panic on the sync path. The transaction
		// rolls back and the row keeps its previous state (it stays in the
		// backlog for the next sweep).
		return false, false, errors.New("gitprovider: branch-ref sync function returned no record")
	}

	if p.Close {
		// Close: record the final head BEFORE deletion semantics are
		// already applied (the syncer read it before deleting); an empty
		// record keeps the row's last known head (the ref was already
		// gone). closed is terminal (00031 guard).
		if _, err := tx.Exec(ctx,
			`UPDATE git_branch_refs SET sync_state = 'closed', closed_at = now(), updated_at = now(),
			        head_sha = CASE WHEN $2 <> '' THEN $2 ELSE head_sha END
			 WHERE branch_id = $1`,
			branchID, rec.HeadSHA); err != nil {
			return false, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return false, false, err
		}
		return true, false, nil
	}

	// Create: fork_sha records the SEMANTIC fork point when the canonical
	// store had one (the base state's git_commit_sha). The default-branch
	// fallback is not a sha — it stays NULL, meaning "forked the
	// repository's default branch" (the pre-T0305 path).
	if _, err := tx.Exec(ctx,
		`UPDATE git_branch_refs SET sync_state = 'synced', synced_at = now(), updated_at = now(),
		        fork_sha = CASE WHEN $2 <> '' THEN $2 ELSE fork_sha END,
		        head_sha = $3
		 WHERE branch_id = $1`,
		branchID, rec.ForkSHA, rec.HeadSHA); err != nil {
		return false, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, false, err
	}
	return true, false, nil
}
