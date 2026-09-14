package gitprovider

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The branch semantic completeness flag (T0306): the stored projection the
// PR/merge gates read (migration 00042). Its only writer is the push
// ingestion store — the derivation walks the append-only ingestion
// evidence, so the flag is always a function of facts that can never be
// rewritten. See 00042's header for the full contract.

// BranchSemanticState is one of the two canonical values of the branch
// semantic completeness flag (git_branch_semantic_states.semantic_state,
// docs/16 §4: "mark semantic_complete or unstructured_changes").
type BranchSemanticState string

const (
	// BranchSemanticComplete: the pushed content at the ref's head pointer
	// is fully understood — every changed path resolves to a known
	// scientific manifest, a removal, or no record at all.
	BranchSemanticComplete BranchSemanticState = "semantic_complete"
	// BranchSemanticUnstructured: at least one pushed path at the head is
	// an unresolved unstructured change (docs/16 §4: unstructured_changes).
	// The branch cannot open a formal PR or merge until the evidence
	// resolves it.
	BranchSemanticUnstructured BranchSemanticState = "unstructured_changes"
)

// refreshBranchSemanticState recomputes the branch's flag from the
// ingestion evidence at the ref's CURRENT head pointer and upserts the
// projection row. It runs inside the delivery transaction, after the head
// advance, so the flag and the head pointer can never diverge observably:
// when the advance was refused, the head is still the previous commit and
// the derivation walks that — the refused delivery's rows are facts, but
// they are not part of the head's chain and do not move the flag.
func (s *PGPushIngestStore) refreshBranchSemanticState(ctx context.Context, tx pgx.Tx, giteaRepoID int64, gitRef, branchID string) error {
	var headSHA string
	err := tx.QueryRow(ctx,
		`SELECT COALESCE(head_sha, '') FROM git_branch_refs WHERE branch_id = $1`,
		branchID).Scan(&headSHA)
	if err != nil {
		return fmt.Errorf("gitprovider: read branch head for semantic state: %w", err)
	}
	state := BranchSemanticComplete
	if headSHA != "" {
		outstanding, err := s.outstandingUnstructured(ctx, tx, giteaRepoID, gitRef, headSHA)
		if err != nil {
			return err
		}
		if outstanding {
			state = BranchSemanticUnstructured
		}
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO git_branch_semantic_states (branch_id, semantic_state, updated_at)
		 VALUES ($1, $2, now())
		 ON CONFLICT (branch_id) DO UPDATE
		   SET semantic_state = EXCLUDED.semantic_state, updated_at = now()`,
		branchID, string(state)); err != nil {
		return fmt.Errorf("gitprovider: upsert branch semantic state: %w", err)
	}
	return nil
}

// outstandingUnstructured reports whether the pushed-head chain of the ref,
// walked from headSHA backwards over the ingestion rows' (before, after)
// links, carries any path whose nearest-to-head change record is an
// unstructured add or modify. A path is resolved by a nearer record of any
// other shape: a semantic manifest at the same path (the content was
// replaced by something the platform understands) or a removal (the file
// is gone). A chain link without an ingestion row — a head set by the ref
// syncer without a delivery, or the fork point — contributes no evidence
// and ends the walk: the flag reports only what the recorded evidence
// shows, and missing evidence is not evidence of unstructured content.
//
// The walk terminates: each hop consumes one distinct after value (the
// dedupe key makes rows unique per after) and the visited set breaks
// before-links that form a cycle (a crafted payload pair can claim each
// other as parent — the payload's SHAs are delivery claims, not provider
// facts).
func (s *PGPushIngestStore) outstandingUnstructured(ctx context.Context, tx pgx.Tx, giteaRepoID int64, gitRef, headSHA string) (bool, error) {
	cur := headSHA
	visited := map[string]bool{}
	seen := map[string]bool{}
	for cur != "" && !isZerosSHA(cur) && !visited[cur] {
		visited[cur] = true
		var ingID, before string
		err := tx.QueryRow(ctx,
			`SELECT id::text, before_sha FROM git_push_ingestions
			  WHERE gitea_repo_id = $1 AND git_ref = $2 AND after_sha = $3`,
			giteaRepoID, gitRef, cur).Scan(&ingID, &before)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("gitprovider: walk ingested push chain: %w", err)
		}
		rows, err := tx.Query(ctx,
			`SELECT path, change_kind, file_kind FROM git_push_changes
			  WHERE ingestion_id = $1 ORDER BY path`, ingID)
		if err != nil {
			return false, fmt.Errorf("gitprovider: read pushed changes for semantic state: %w", err)
		}
		var outstanding bool
		for rows.Next() {
			var path, kind, fileKind string
			if err := rows.Scan(&path, &kind, &fileKind); err != nil {
				rows.Close()
				return false, fmt.Errorf("gitprovider: scan pushed change for semantic state: %w", err)
			}
			if seen[path] {
				continue // a nearer-to-head record already decided this path
			}
			seen[path] = true
			if fileKind == string(FileKindUnstructured) && kind != string(ChangeRemoved) {
				outstanding = true
				break
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return false, fmt.Errorf("gitprovider: iterate pushed changes for semantic state: %w", err)
		}
		if outstanding {
			return true, nil
		}
		if before == cur {
			break // a delivery claiming itself as parent — the chain ends here
		}
		cur = before
	}
	return false, nil
}
