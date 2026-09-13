-- RSG state model (canonical tables: branches, project_states, state_commits).
-- Branch = research state evolution path; commit = state transition
-- (docs/03_GLOSSARY_DOMAIN_MODEL.md, invariants 2-4).

-- CreateBranch is the raw, unguarded insert: seeding fixtures and the
-- canonical headless form (base_state_id NULL — a branch before its first
-- state). The domain creation path is CreateBranchFromState below, which
-- re-checks the fork point's project inside the insert.
-- name: CreateBranch :one
INSERT INTO branches (project_id, name, visibility, purpose, git_ref, base_state_id, created_by)
VALUES (@project_id, @name, @visibility, @purpose, @git_ref, @base_state_id, @created_by)
RETURNING *;

-- name: CreateBranchFromState :one
-- Guarded insert (T0205): a branch always forks a state of the SAME
-- project — the EXISTS re-checks the fork point inside the insert, so a
-- base state of another project (or a missing one) yields zero rows
-- instead of a row, and the adapter reports ErrBaseStateNotFound for
-- both without leaking which state exists where. The project row itself is
-- read (and visibility-defaulted against) in the same transaction by the
-- adapter.
INSERT INTO branches (project_id, name, visibility, purpose, git_ref, base_state_id, created_by)
SELECT @project_id, @name, @visibility, @purpose, @git_ref, @base_state_id, @created_by
WHERE EXISTS (
  SELECT 1 FROM project_states
  WHERE id = @base_state_id AND project_id = @project_id
)
RETURNING *;

-- name: GetBranchByID :one
SELECT * FROM branches WHERE id = @id;

-- name: GetBranchByProjectAndID :one
-- Project-scoped read: a branch id of another project matches nothing and
-- reports the same "not found" outcome (never leak another project's
-- entity existence, docs/45).
SELECT * FROM branches WHERE id = @id AND project_id = @project_id;

-- name: ListBranchesByProject :many
SELECT * FROM branches
WHERE project_id = @project_id
ORDER BY created_at, id;

-- name: SetBranchLifecycle :one
-- The lifecycle compare-and-swap (T0205): active → merged | aborted is
-- the only transition (docs/43), terminal once made. Zero rows mean
-- either the branch is not in the project, it is main (protected — its
-- lifecycle is the project's), or it already closed; the adapter
-- distinguishes by one read.
UPDATE branches
SET lifecycle_state = @lifecycle_state
WHERE id = @id
  AND project_id = @project_id
  AND name <> 'main'
  AND lifecycle_state = 'active'
RETURNING *;

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
--
-- T0205 adds the lifecycle guard: a merged/aborted branch's head never
-- moves (docs/43: history immutable), so the CAS also requires the branch
-- to be active. The adapter's read after zero rows reports that case as
-- *states.BranchNotActiveError instead of a conflict.
-- name: UpdateBranchBaseState :one
UPDATE branches
SET base_state_id = @base_state_id
WHERE id = @id
  AND project_id = @project_id
  AND lifecycle_state = 'active'
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
