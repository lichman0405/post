-- Scientific objects and their append-only version log (canonical tables:
-- scientific_objects, scientific_object_versions). Historical content is never
-- UPDATEd in place; a new version row is inserted instead (docs/53).

-- name: CreateScientificObject :one
INSERT INTO scientific_objects (project_id, object_type, created_by)
VALUES (@project_id, @object_type, @created_by)
RETURNING *;

-- name: GetScientificObjectByID :one
SELECT * FROM scientific_objects WHERE id = @id;

-- name: CreateScientificObjectVersion :one
INSERT INTO scientific_object_versions
    (object_id, version_no, state_id, branch_id, schema_id, schema_version,
     title, lifecycle_state, payload, visibility_policy_id, integrity_hash, created_by)
VALUES
    (@object_id, @version_no, @state_id, @branch_id, @schema_id, @schema_version,
     @title, @lifecycle_state, @payload, @visibility_policy_id, @integrity_hash, @created_by)
RETURNING *;

-- name: GetScientificObjectVersionByID :one
SELECT * FROM scientific_object_versions WHERE id = @id;

-- name: GetScientificObjectVersionByNo :one
SELECT * FROM scientific_object_versions
WHERE object_id = @object_id AND version_no = @version_no;

-- name: GetLatestScientificObjectVersion :one
SELECT * FROM scientific_object_versions
WHERE object_id = @object_id
ORDER BY version_no DESC
LIMIT 1;

-- name: ListScientificObjectVersions :many
SELECT * FROM scientific_object_versions
WHERE object_id = @object_id
ORDER BY version_no;
