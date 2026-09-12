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

-- name: ListPullRequestsByProject :many
SELECT * FROM pull_requests
WHERE project_id = @project_id
ORDER BY number;

-- name: CreateReview :one
INSERT INTO reviews (pull_request_id, reviewer_id, review_kind, decision, body)
VALUES (@pull_request_id, @reviewer_id, @review_kind, @decision, @body)
RETURNING *;
