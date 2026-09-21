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
// git_repository_provisions.webhook_secret — plus, for the frozen-main rule
// (T0601), the delivery's verdict read (projects.main_frozen and the merge
// commit the project's main has recorded) — and the only writer of the
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

// MainPushVerdict implements IngestStore (T0601): the project's main_frozen
// flag and whether afterSHA is the merge commit the platform itself
// recorded for that project's main — both read through the provision row
// the repository id is keyed by, in one statement. A repository id no
// provision row carries answers the zero verdict: the ingestion's handling
// of an unprovisioned delivery is T0305's and this rule does not change it.
//
// The merge side is the seam T0601 turned out to need. The platform merges
// through the provider's API (T0409), and the provider delivers that merge
// as a push of refs/heads/main with no merge marker, onto the very
// endpoint this store feeds — so "frozen main accepts nothing" would refuse
// the platform's own governed merge and leave main's recorded head behind
// for ever. The record that tells its own merge apart from a foreign push
// is the saga's: semantic_merges.git_sha, written by CompleteGitStep when
// the provider merge landed, on the ref the merge targeted (`git_ref` is
// the same refs/heads/<name> projection branches carry, and the merge's
// target branch is pinned by target_branch_id).
//
// The row read is the NEWEST merge whose Git step completed on main, so the
// seam names the commit main is at, not every commit the platform ever
// merged: a push whose after rewinds main onto an older recorded merge
// commit does not match and is refused (fail-closed — anything the store
// cannot prove is the current merged head answers false). Ordered by
// created_at, id — the (project_id, created_at DESC, id) index of migration
// 00069 — with the `updated` filter applied before the ordering, so a merge
// still in flight never hides the last completed one.
//
// A note on ordering, because the seam's value depends on it: the provider
// call (and therefore the delivery) precedes CompleteGitStep, so a delivery
// that wins that race finds no record yet and IS refused. That is the
// designed fail-closed answer — no record, no seam — and it is not silent:
// main's recorded head then trails the ref, which is exactly the drift
// T0309's reconciler reports (`ref_head_moved`) and holds a repair proposal
// for, the same recovery it offers a push whose webhook was lost. The
// alternative — admitting a main push because a merge LOOKS in flight —
// would admit any foreign push made during a merge window, which is a hole
// in the freeze rather than a race in it.
func (s *PGPushIngestStore) MainPushVerdict(ctx context.Context, giteaRepoID int64, afterSHA string) (MainPushVerdict, error) {
	var verdict MainPushVerdict
	err := s.pool.QueryRow(ctx,
		`SELECT p.main_frozen,
		        (m.git_sha IS NOT NULL AND m.git_sha = $2)
		   FROM git_repository_provisions r
		   JOIN projects p ON p.id = r.project_id
		   LEFT JOIN LATERAL (
		        SELECT git_sha
		          FROM semantic_merges
		         WHERE project_id = r.project_id
		           AND git_ref = $3
		           AND git_state = 'updated'
		         ORDER BY created_at DESC, id DESC
		         LIMIT 1
		   ) m ON TRUE
		  WHERE r.gitea_repo_id = $1`, giteaRepoID, afterSHA, MainRef).
		Scan(&verdict.Frozen, &verdict.PlatformMerge)
	if errors.Is(err, pgx.ErrNoRows) {
		return MainPushVerdict{}, nil
	}
	if err != nil {
		return MainPushVerdict{}, err
	}
	return verdict, nil
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
//     itself ingested;
//   - the branch's semantic completeness flag (git_branch_semantic_states,
//     T0306) — recomputed from the change rows at the head pointer, so the
//     PR/merge gates of migration 00042 always see the flag that matches
//     the recorded head.
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
	//
	// A PUSH and a FORK IMPORT record the same row for different reasons,
	// and the difference is the parent:
	//
	//   - a push chains the pushed head to the state of the commit it was
	//     pushed on (resolved from `before`), and moves no branch pointer —
	//     the ref pointer above is the only thing a push moves;
	//   - the fork import (in.ForkImport, T0817) LANDS the branch on the
	//     copied content, so the pushed head is the branch's next state and
	//     is chained to what the branch stands on (its base_state_id, read
	//     here inside the delivery's own transaction). A copy is the
	//     branch's first content, so `before` is zeros and the push rule
	//     has no state to name; the branch row does, and it is the honest
	//     base — the chain then walks from the branch's head back through
	//     the copy to the state the branch was created from, which is
	//     exactly the boundary the integrity review resolves
	//     (rsg/integrity: "the state the chain's root builds on — the
	//     branches.base_state_id at fork time"). Without it the copied
	//     state is a state of the branch that no commit names and no
	//     pointer reaches, and every proposal from the branch is refused
	//     for having two heads.
	if branchID != nil && !isZerosSHA(ev.After) {
		var parentID *string
		switch {
		case in.ForkImport != nil:
			if err := tx.QueryRow(ctx,
				`SELECT base_state_id::text FROM branches WHERE id = $1`,
				*branchID).Scan(&parentID); err != nil {
				return false, err
			}
		case !isZerosSHA(ev.Before):
			err = tx.QueryRow(ctx,
				`SELECT id::text FROM project_states
				  WHERE project_id = $1 AND git_commit_sha = $2
				  ORDER BY created_at, id LIMIT 1`,
				*projectID, ev.Before).Scan(&parentID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return false, err
			}
		}
		stateID, err := upsertPushedState(ctx, tx, *projectID, *branchID, parentID, ev.After, in.ForkImport != nil)
		if err != nil {
			return false, err
		}
		if in.ForkImport != nil {
			if err := recordForkImportTransition(ctx, tx, *projectID, *branchID, parentID, stateID, *in.ForkImport); err != nil {
				return false, err
			}
		}
	}

	// T0306: the branch's semantic completeness flag is a projection of the
	// ingestion evidence at the head pointer — refresh it inside the same
	// transaction, AFTER the advance, so flag and head can never diverge
	// observably. When the advance was refused the head is still the
	// previous commit and the derivation walks that (the refused delivery's
	// rows are facts, but they are not part of the head's chain).
	if branchID != nil && !isZerosSHA(ev.After) {
		if err := s.refreshBranchSemanticState(ctx, tx, ev.RepositoryID, ev.Ref, *branchID); err != nil {
			return false, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// upsertPushedState records a delivery's head as a project state and returns
// the row's id. The state is content-addressed by the commit
// (UNIQUE(project_id, state_hash)), so the same commit on another branch of
// the project REUSES the row instead of writing a second one — the
// documented behaviour of this path (see the contract on IngestPush above),
// and the reason a conflict is not an error here; the read-back makes the id
// available either way, which is what lets the fork import chain the branch
// to it.
//
// ownBranchRequired narrows that for the ONE caller that cannot live with the
// reuse: the fork import (in.ForkImport, T0817). That caller LANDS its branch
// on the copied content — recordForkImportTransition moves the head to this
// state and writes the commit naming it — so a row owned by a different
// branch would make the fork branch's chain run through another branch's
// state and the transition name a state this branch never authored. With the
// flag set, a foreign-owned row is refused and the delivery rolls back. A
// plain push never sets it: `git push origin main:<branch>` — a commit that
// is already a state of the project arriving as another branch's head — is
// the reuse this path documents, and it stays a success.
func upsertPushedState(ctx context.Context, tx pgx.Tx, projectID, branchID string, parentID *string, after string, ownBranchRequired bool) (string, error) {
	hash := GitStateHash(after)
	var id string
	err := tx.QueryRow(ctx,
		`INSERT INTO project_states
		   (project_id, branch_id, parent_state_id, state_hash, git_commit_sha, manifest_version)
		 VALUES ($1, $2, $3, $4, $5, 'v1')
		 ON CONFLICT (project_id, state_hash) DO NOTHING
		 RETURNING id::text`,
		projectID, branchID, parentID, hash, after).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	// The commit is already a state of this project: the push path REUSES
	// the row (that is its documented contract), while the fork import
	// refuses one it does not own — its transition would otherwise name a
	// state of a foreign chain.
	var owner *string
	if err := tx.QueryRow(ctx,
		`SELECT id::text, branch_id::text FROM project_states
		  WHERE project_id = $1 AND state_hash = $2`,
		projectID, hash).Scan(&id, &owner); err != nil {
		return "", err
	}
	if ownBranchRequired && (owner == nil || *owner != branchID) {
		return "", fmt.Errorf(
			"gitprovider: the commit %s is already the state of another branch in project %s, so the fork import cannot land it as a state of this branch",
			after, projectID)
	}
	return id, nil
}

// recordForkImportTransition makes a content copy a STATE TRANSITION rather
// than only a fact (T0817): the branch MOVES to the state that carries the
// copied content, and the move is recorded the way every other transition is
// (docs/09 §2: a state commit names actor, via, message and the base →
// result pair). Without it the copy leaves a state of the branch that no
// commit names and no pointer reaches — a chain with two heads, which the
// integrity review refuses, rightly, for every proposal from the branch.
//
// Both writes are guarded the way this package guards every pointer:
//
//   - the head advance is a compare-and-swap against the base the delivery
//     read, with IS NOT DISTINCT FROM so that a branch standing on no state
//     at all is matched rather than silently skipped. Zero rows means the
//     branch moved underneath the copy: the delivery is refused and the
//     whole transaction rolls back, because recording the copy while the
//     branch points elsewhere is precisely the incoherent history this
//     function exists to prevent;
//   - the commit row is written only while no commit already names this
//     result state ON THIS BRANCH, so re-recording one transition cannot
//     make it two (the integrity review reads one commit per state).
//
// The actor is the person the platform made the copy for, never the
// platform: state_commits.actor_id is NOT NULL and the copy is performed on
// the forker's behalf (the delivery's pusher says the same on the Git side).
// The via is 'git_compat' — the content arrived through the Git side, which
// is the channel the vocabulary has for it (00004).
func recordForkImportTransition(ctx context.Context, tx pgx.Tx, projectID, branchID string, baseStateID *string, stateID string, tr ForkImportTransition) error {
	tag, err := tx.Exec(ctx,
		`UPDATE branches SET base_state_id = $1
		  WHERE id = $2 AND base_state_id IS NOT DISTINCT FROM $3`,
		stateID, branchID, baseStateID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf(
			"gitprovider: the branch moved while the copy was being recorded (it no longer stands on %v), so the copy was refused rather than landing on a history it does not describe",
			derefOrNil(baseStateID))
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO state_commits
		   (project_id, branch_id, base_state_id, result_state_id, actor_id, via, message, operation_summary)
		 SELECT $1, $2, $3, $4, $5, 'git_compat', $6, '[]'::jsonb
		  WHERE NOT EXISTS (
		        SELECT 1 FROM state_commits
		         WHERE branch_id = $2 AND result_state_id = $4)`,
		projectID, branchID, baseStateID, stateID, tr.ActorID, tr.Message); err != nil {
		return err
	}
	return nil
}

// derefOrNil is fmt's rendering of a nullable id: the value, or an explicit
// nil, so an error message never reads "stands on " for a branch that stands
// on nothing.
func derefOrNil(id *string) any {
	if id == nil {
		return nil
	}
	return *id
}
