-- Policy versions (canonical table policy_versions; 00033 adds the
-- per-scope version uniqueness and the version shape check). The table is
-- append-only (00014/00015): these queries INSERT and SELECT only — a
-- policy change is a new version row, never an UPDATE.

-- name: CreatePolicyVersion :one
INSERT INTO policy_versions (organization_id, project_id, version, policy_json, created_by)
VALUES (@organization_id, @project_id, @version, @policy_json, @created_by)
RETURNING *;

-- name: GetPolicyVersion :one
-- One version by id — any age: old versions stay queryable forever
-- (T0603 acceptance).
SELECT * FROM policy_versions WHERE id = @id;

-- name: ListPolicyVersionsByOrg :many
-- Every version of the organization, newest first (created_at, then id
-- as a deterministic tie-break).
SELECT * FROM policy_versions
WHERE organization_id = @organization_id
ORDER BY created_at DESC, id DESC;

-- name: ListPolicyVersionsByProject :many
SELECT * FROM policy_versions
WHERE project_id = @project_id
ORDER BY created_at DESC, id DESC;

-- name: LatestPolicyVersionByOrg :one
-- The organization's current policy (the project lower bound).
SELECT * FROM policy_versions
WHERE organization_id = @organization_id
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: LatestPolicyVersionByProject :one
SELECT * FROM policy_versions
WHERE project_id = @project_id
ORDER BY created_at DESC, id DESC
LIMIT 1;
