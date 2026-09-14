package gitprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The canonical-store side of push ingestion (T0305): the only reader of
// git_repository_provisions.webhook_secret and the only writer of the
// git_push_* tables (plus the two pointers ingestion maintains:
// git_branch_refs.head_sha — T0303 assigned the freshness to this task —
// and the pushed-head project_states rows the branch fork-point
// resolution reads). Plain pgx, same discipline as the provisioning and
// branch-ref stores: these columns belong to this package, sqlc queries
// would put them on the shared persistence surface.

// PGPushIngestStore is the PostgreSQL adapter for the push ingestion
// tables. It implements IngestStore.
type PGPushIngestStore struct {
	pool *pgxpool.Pool
}

// NewPushIngestStore builds the store on pool. The pool may be lazy
// (persistence.OpenLazy): the API keeps starting while PostgreSQL is down.
func NewPushIngestStore(pool *pgxpool.Pool) *PGPushIngestStore {
	return &PGPushIngestStore{pool: pool}
}

// WebhookSecretByRepoID implements IngestStore. The secret lives in the
// canonical store because the receiver must verify every delivery against
// it (docs/55 SECRET) — it exists in no other read path.
func (s *PGPushIngestStore) WebhookSecretByRepoID(ctx context.Context, giteaRepoID int64) (string, error) {
	var secret string
	err := s.pool.QueryRow(ctx,
		`SELECT webhook_secret FROM git_repository_provisions WHERE gitea_repo_id = $1`,
		giteaRepoID).Scan(&secret)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: gitea repository id %d", ErrRepoNotProvisioned, giteaRepoID)
	}
	if err != nil {
		return "", err
	}
	return secret, nil
}

// IngestPush implements IngestStore: one delivery and its inspection
// result become rows in ONE transaction, or nothing becomes anything —
// the dedupe key (gitea_repo_id, git_ref, after_sha) is checked by insert,
// and a duplicate delivery is a complete no-op (the acceptance criterion
// "重复 webhook 不重复 state": the state, the changes, the candidates and
// the head pointer are all written exactly once per pushed head).
//
// Inside the transaction the store also derives what the service cannot:
// the canonical project (from the provision row), the branch the ref
// names (if one exists — an out-of-band ref is recorded but maps to no
// branch), the parent state (the ingested state of the before commit,
// when there is one), and then commits:
//
//   - git_branch_refs.head_sha ← after, GUARDED: the pointer advances
//     only when it is still where this push started (head_sha =
//     before_sha) or — for a creation push (before is zeros) — only
//     while the ref is still unborn (head_sha IS NULL) or already at the
//     pushed head; a refused advance is still recorded in full, but
//     visibly, as head_skip_reason = 'stale_before' / 'stale_creation'
//     on the row — a stale redelivery of an older push can never rewind
//     the pointer past a newer head;
//   - the ingestion row (ON CONFLICT DO NOTHING — no row = duplicate;
//     the deferred rollback discards the speculative advance, so a
//     duplicate is a complete no-op including the pointer);
//   - one git_push_changes row per changed path;
//   - one git_push_semantic_candidates row per manifest candidate;
//   - a project_states row for the pushed head (ON CONFLICT on the
//     content hash — the same commit on another branch reuses the same
//     state), chained to the parent state when the before commit was
//     itself ingested.
func (s *PGPushIngestStore) IngestPush(ctx context.Context, in IngestPushParams) (bool, error) {
	ev := in.Event
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Canonical project: NULL for a delivery naming an unprovisioned
	// repository — the delivery is still recorded (audit), it just maps
	// to no branch and no state.
	var projectID *string
	err = tx.QueryRow(ctx,
		`SELECT project_id::text FROM git_repository_provisions WHERE gitea_repo_id = $1`,
		ev.RepositoryID).Scan(&projectID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}

	// The branch the ref names, when the project owns one (branches.git_ref
	// is derived 1:1 from the name — 00031's guard).
	var branchID *string
	if projectID != nil {
		err = tx.QueryRow(ctx,
			`SELECT id::text FROM branches WHERE project_id = $1 AND git_ref = $2`,
			*projectID, ev.Ref).Scan(&branchID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
	}

	// The head-pointer advance runs BEFORE the ingestion insert, and it is
	// guarded in BOTH paths — the pointer is not a free fact of the push:
	//   - a push whose before names the current head advances only while
	//     the pointer is still there (head_sha = before_sha) — a delivery
	//     whose before no longer matches is refused;
	//   - a creation push (before is zeros) advances only while the ref is
	//     still unborn (head_sha IS NULL) or already at the pushed head —
	//     a creation delivery arriving after the ref gained a head (a
	//     later sync, or a newer push) is refused too.
	// A refused advance is recorded VISIBLY, never as a silent zero-row
	// update: the delivery's audit rows are still recorded below, and the
	// row carries head_skip_reason ('stale_before' / 'stale_creation') —
	// an older push whose first attempt 503'd and whose redelivery arrives
	// after a newer push was ingested can never rewind the pointer past
	// the newer head, in either path. The advance is also speculative:
	// when the insert below turns out to be an exact duplicate, the
	// deferred rollback discards it, so a duplicate delivery stays a
	// complete no-op including the pointer.
	var headSkipReason *string
	if branchID != nil && !isZerosSHA(ev.After) {
		if isZerosSHA(ev.Before) {
			// Ref creation: the pointer lands on the pushed head only
			// while the ref is still unborn or already at that head — a
			// redelivered creation push whose ref has since gained a
			// newer head must not rewind it. Zero affected rows means the
			// ref already carried a different head (00031's
			// branch_git_ref_map trigger makes an unmapped branch
			// impossible, so the row always exists).
			if tag, err := tx.Exec(ctx,
				`UPDATE git_branch_refs SET head_sha = $1
				  WHERE branch_id = $2 AND (head_sha IS NULL OR head_sha = $1)`,
				ev.After, *branchID); err != nil {
				return false, err
			} else if tag.RowsAffected() == 0 {
				reason := "stale_creation"
				headSkipReason = &reason
			}
		} else if tag, err := tx.Exec(ctx,
			`UPDATE git_branch_refs SET head_sha = $1 WHERE branch_id = $2 AND head_sha = $3`,
			ev.After, *branchID, ev.Before); err != nil {
			return false, err
		} else if tag.RowsAffected() == 0 {
			// The pointer is no longer where this push started: a stale
			// delivery. Zero affected rows means exactly that — 00031's
			// branch_git_ref_map trigger makes an unmapped branch
			// impossible, so the row always exists.
			reason := "stale_before"
			headSkipReason = &reason
		}
	}

	commits := json.RawMessage("[]")
	if len(ev.Commits) > 0 {
		if b, err := json.Marshal(ev.Commits); err == nil {
			commits = b
		}
	}
	pusher := any(nil)
	if ev.Pusher != "" {
		pusher = ev.Pusher
	}
	var ingestionID string
	err = tx.QueryRow(ctx,
		`INSERT INTO git_push_ingestions
		   (delivery_id, gitea_repo_id, project_id, branch_name, git_ref,
		    before_sha, after_sha, commit_count, pusher, commits, head_skip_reason)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		 ON CONFLICT (gitea_repo_id, git_ref, after_sha) DO NOTHING
		 RETURNING id::text`,
		ev.DeliveryID, ev.RepositoryID, projectID,
		strings.TrimPrefix(ev.Ref, "refs/heads/"), ev.Ref,
		ev.Before, ev.After, ev.TotalCommits, pusher, commits, headSkipReason).Scan(&ingestionID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Duplicate delivery: the push was already ingested. Nothing is
		// committed — the deferred rollback also discards the speculative
		// head advance above, so a duplicate is a complete no-op and a
		// stale redelivery can never move the pointer.
		return false, nil
	}
	if err != nil {
		return false, err
	}

	for _, c := range in.Changes {
		schemaID := any(nil)
		contentSHA := any(nil)
		if c.File == FileKindManifest {
			schemaID = c.SchemaID
			contentSHA = c.ContentSHA256
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO git_push_changes
			   (ingestion_id, path, change_kind, file_kind, schema_id, content_sha256)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			ingestionID, c.Path, string(c.Kind), string(c.File), schemaID, contentSHA); err != nil {
			return false, err
		}
	}
	for _, cand := range in.Candidates {
		if _, err := tx.Exec(ctx,
			`INSERT INTO git_push_semantic_candidates
			   (ingestion_id, path, change_kind, schema_id, candidate)
			 VALUES ($1, $2, $3, $4, $5)`,
			ingestionID, cand.Path, string(cand.Kind), cand.SchemaID, cand.Content); err != nil {
			return false, err
		}
	}

	// The pushed-head state row. Skipped when the ref maps to no branch
	// (an out-of-band ref) or the delivery carried no head (a deletion —
	// defensive on this instance, which never delivers deletions as push
	// events). The state is the commit's content identity — it is recorded
	// even when the head advance was refused: the push happened, the
	// commit exists, and append-only history keeps it (only the pointer is
	// guarded, never the facts of the delivery).
	if branchID != nil && !isZerosSHA(ev.After) {
		var parentID *string
		if !isZerosSHA(ev.Before) {
			err = tx.QueryRow(ctx,
				`SELECT id::text FROM project_states
				  WHERE project_id = $1 AND git_commit_sha = $2
				  ORDER BY created_at, id LIMIT 1`,
				*projectID, ev.Before).Scan(&parentID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return false, err
			}
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO project_states
			   (project_id, branch_id, parent_state_id, state_hash, git_commit_sha, manifest_version)
			 VALUES ($1, $2, $3, $4, $5, 'v1')
			 ON CONFLICT (project_id, state_hash) DO NOTHING`,
			*projectID, *branchID, parentID, GitStateHash(ev.After), ev.After); err != nil {
			return false, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
