-- Issues, pull requests, reviews (canonical tables: issues, pull_requests,
-- reviews). A research PR is a proposed RSG diff; merge controls acceptance
-- (invariant 6).

-- name: CreateIssue :one
INSERT INTO issues (project_id, number, issue_type, title, body, created_by)
VALUES (@project_id, @number, @issue_type, @title, @body, @created_by)
RETURNING *;

-- name: GetIssueByProjectAndNumber :one
SELECT * FROM issues
WHERE project_id = @project_id AND number = @number;

-- name: NextIssueNumber :one
-- The per-project issue number allocator (T0811, the first caller of
-- CreateIssue). Same shape as the PR store's own allocation: the
-- INSERT ... SELECT pair runs inside one transaction that has already
-- row-locked the project row (GetProjectByIDForUpdate), so MAX(number)+1
-- cannot race a concurrent create and issues(project_id, number) is never
-- violated. Numbers start at 1 for every project.
SELECT (COALESCE(MAX(number), 0) + 1)::bigint AS number FROM issues WHERE project_id = @project_id;

-- name: CreatePullRequest :one
-- creation_key is the creation request's Idempotency-Key (migration 00089):
-- empty when the caller sent none, and UNIQUE per project when it did not.
INSERT INTO pull_requests
    (project_id, number, source_branch_id, target_branch_id,
     base_state_id, proposed_state_id, title, body, created_by, creation_key)
VALUES
    (@project_id, @number, @source_branch_id, @target_branch_id,
     @base_state_id, @proposed_state_id, @title, @body, @created_by, @creation_key)
RETURNING *;

-- name: GetPullRequestByProjectAndNumber :one
SELECT * FROM pull_requests
WHERE project_id = @project_id AND number = @number;

-- name: GetPullRequestByCreationKey :one
-- The creation replay read (T0410, migration 00089): the proposal a previous
-- request with this Idempotency-Key opened. Only a non-empty key names
-- anything — '' is "no key", shared by every key-less row, so it is excluded
-- here rather than left to the partial index.
SELECT * FROM pull_requests
WHERE project_id = @project_id AND creation_key = @creation_key AND creation_key <> '';

-- name: GetPullRequestByProjectAndNumberForUpdate :one
-- The refresh/transition row lock (T0402): serializes the head refresh
-- against concurrent state transitions inside one transaction.
SELECT * FROM pull_requests
WHERE project_id = @project_id AND number = @number
FOR UPDATE;

-- name: SetPullRequestState :one
-- The state transition compare-and-swap (T0402, docs/43): the update
-- matches only while the row is still in the expected state, so a
-- concurrent transition fails the CAS instead of overwriting it.
-- Migration 00051's pull_request_guard enforces the transition map and
-- merged_at consistency itself; this CAS is the application-side
-- serialization on top.
UPDATE pull_requests
SET state = @next_state,
    merged_at = CASE WHEN @next_state = 'merged' THEN now() ELSE NULL END
WHERE project_id = @project_id AND number = @number AND state = @expected
RETURNING *;

-- name: EnablePullRequestHeadRefresh :exec
-- The transaction-scoped session flag migration 00051's fixity guard
-- requires for the explicit head refresh (set_config is_local=true
-- resets at transaction end): the proposed state moves ONLY through the
-- flagged path, whatever else runs in the database.
SELECT set_config('post.pr_head_refresh', 'on', true);

-- name: RefreshPullRequestProposedState :one
-- The explicit head refresh (T0402, acceptance "head update 可显式
-- refresh"): re-points proposed_state_id to the source branch's current
-- head. The adapter runs it inside one transaction with the row locked
-- and the session flag on — the only sanctioned write path for the
-- proposed state.
UPDATE pull_requests
SET proposed_state_id = @proposed_state_id
WHERE project_id = @project_id AND number = @number
RETURNING *;

-- name: ListPullRequestsByProject :many
SELECT * FROM pull_requests
WHERE project_id = @project_id
ORDER BY number;

-- name: CreateReview :one
-- One per-dimension review decision about one proposed head (T0404,
-- migration 00061): reviewed_state_id is the exact head the reviewer
-- evaluated (derived from the PR's proposed_state_id inside the
-- submission transaction, never caller-supplied) and responsibility is
-- the reviewer-responsibility label the service resolved (docs/04 §3;
-- empty when none). The unique constraint scopes one decision per
-- (PR, reviewer, kind, head) — a duplicate decision about the same head
-- is refused, while different kinds and later heads record freely.
INSERT INTO reviews
    (pull_request_id, reviewer_id, review_kind, decision, reviewed_state_id, responsibility, body)
VALUES
    (@pull_request_id, @reviewer_id, @review_kind, @decision, @reviewed_state_id, @responsibility, @body)
RETURNING *;

-- name: ListReviewsByPullRequest :many
-- Every review of one PR (project-scoped through the PR row), oldest
-- first (created_at, id — a total order; the release record reads
-- reviews in the same order).
SELECT r.*
FROM reviews r
JOIN pull_requests pr ON pr.id = r.pull_request_id
WHERE pr.project_id = @project_id AND pr.number = @number
ORDER BY r.created_at, r.id;
