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

-- name: CreatePullRequest :one
INSERT INTO pull_requests
    (project_id, number, source_branch_id, target_branch_id,
     base_state_id, proposed_state_id, title, body, created_by)
VALUES
    (@project_id, @number, @source_branch_id, @target_branch_id,
     @base_state_id, @proposed_state_id, @title, @body, @created_by)
RETURNING *;

-- name: GetPullRequestByProjectAndNumber :one
SELECT * FROM pull_requests
WHERE project_id = @project_id AND number = @number;

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
INSERT INTO reviews (pull_request_id, reviewer_id, review_kind, decision, body)
VALUES (@pull_request_id, @reviewer_id, @review_kind, @decision, @body)
RETURNING *;
