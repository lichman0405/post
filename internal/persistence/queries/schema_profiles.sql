-- Project schema profiles (canonical table project_schema_profiles; 00038).
-- The table is append-only (the 00038 trigger): these queries INSERT and
-- SELECT only — a profile change is a new version row, never an UPDATE.

-- name: InsertSchemaProfile :one
INSERT INTO project_schema_profiles
    (project_id, schema_id, version, base_schema_id, base_schema_version,
     content, content_hash, created_by)
VALUES
    (@project_id, @schema_id, @version, @base_schema_id, @base_schema_version,
     @content, @content_hash, @created_by)
RETURNING *;

-- name: GetSchemaProfile :one
-- One version by id — any age: old versions stay queryable forever, so a
-- profile v2 never invalidates history written under v1 (docs/21 §8).
SELECT * FROM project_schema_profiles
WHERE project_id = @project_id AND schema_id = @schema_id AND version = @version;

-- name: GetLatestSchemaProfile :one
-- The profile's current version — the newest registration, created_at then
-- id as a deterministic tie-break (the object-create path resolves a bare
-- schema id to this row).
SELECT * FROM project_schema_profiles
WHERE project_id = @project_id AND schema_id = @schema_id
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: ListProjectSchemaProfiles :many
-- Every profile version of the project, newest first.
SELECT * FROM project_schema_profiles
WHERE project_id = @project_id
ORDER BY created_at DESC, id DESC;

-- name: ListAllSchemaProfiles :many
-- The startup load: every registered profile row, any project. The API
-- re-registers each into the in-memory schema registry at boot.
SELECT * FROM project_schema_profiles
ORDER BY created_at, id;
