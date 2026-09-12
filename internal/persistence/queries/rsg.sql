-- RSG state model (canonical tables: branches, project_states, state_commits).
-- Branch = research state evolution path; commit = state transition
-- (docs/03_GLOSSARY_DOMAIN_MODEL.md, invariants 2-4).

-- name: CreateBranch :one
INSERT INTO branches (project_id, name, visibility, purpose, git_ref, base_state_id, created_by)
VALUES (@project_id, @name, @visibility, @purpose, @git_ref, @base_state_id, @created_by)
RETURNING *;

-- name: GetBranchByID :one
SELECT * FROM branches WHERE id = @id;

-- name: ListBranchesByProject :many
SELECT * FROM branches
WHERE project_id = @project_id
ORDER BY created_at, id;

-- name: CreateProjectState :one
INSERT INTO project_states (project_id, branch_id, parent_state_id, state_hash, git_commit_sha, manifest_version)
VALUES (@project_id, @branch_id, @parent_state_id, @state_hash, @git_commit_sha, @manifest_version)
RETURNING *;

-- name: GetProjectStateByID :one
SELECT * FROM project_states WHERE id = @id;

-- name: GetProjectStateByHash :one
SELECT * FROM project_states WHERE project_id = @project_id AND state_hash = @state_hash;

-- name: CreateStateCommit :one
INSERT INTO state_commits
    (project_id, branch_id, base_state_id, result_state_id, actor_id, via, message, operation_summary)
VALUES
    (@project_id, @branch_id, @base_state_id, @result_state_id, @actor_id, @via, @message, @operation_summary)
RETURNING *;

-- name: ListStateCommitsByBranch :many
SELECT * FROM state_commits
WHERE branch_id = @branch_id
ORDER BY created_at, id;
