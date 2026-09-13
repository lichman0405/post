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

-- name: GetStateCommitByID :one
SELECT * FROM state_commits WHERE id = @id;

-- UpdateBranchBaseState is the branch head compare-and-swap behind
-- CommitState (T0204): the head pointer advances to the new state only
-- while it still equals the base the commit was built on, so the branch
-- chain stays linear and concurrent commits serialize into one winner and
-- stable BRANCH_STATE_CONFLICT losers. project_id is part of the guard: a
-- branch of another project never matches, and the caller-side read after
-- zero rows reports the same "not found" outcome for it (never leak
-- another project's entity existence).
-- name: UpdateBranchBaseState :one
UPDATE branches
SET base_state_id = @base_state_id
WHERE id = @id
  AND project_id = @project_id
  AND base_state_id IS NOT DISTINCT FROM @expected_base_state_id
RETURNING *;

-- name: ListProjectStatesByBranch :many
SELECT * FROM project_states
WHERE branch_id = @branch_id
ORDER BY created_at, id;

-- The state snapshot projections (docs/21 §5, docs/07 §7): a state's
-- direct members are the version rows whose state_id equals it — the
-- transition each row was created in. Rebuildable from the canonical
-- history by construction.

-- name: ListStateObjectVersionsByState :many
SELECT * FROM scientific_object_versions
WHERE state_id = @state_id
ORDER BY created_at, id;

-- name: ListStateRelationVersionsByState :many
SELECT * FROM relation_versions
WHERE state_id = @state_id
ORDER BY created_at, id;
