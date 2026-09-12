-- Relations between scientific object versions (canonical tables: relations,
-- relation_versions).

-- name: CreateRelation :one
INSERT INTO relations (project_id)
VALUES (@project_id)
RETURNING *;

-- name: CreateRelationVersion :one
INSERT INTO relation_versions
    (relation_id, version_no, state_id, relation_type,
     source_object_version_id, target_object_version_id,
     payload, integrity_hash, created_by)
VALUES
    (@relation_id, @version_no, @state_id, @relation_type,
     @source_object_version_id, @target_object_version_id,
     @payload, @integrity_hash, @created_by)
RETURNING *;

-- name: ListRelationVersionsForSource :many
SELECT * FROM relation_versions
WHERE source_object_version_id = @object_version_id
ORDER BY created_at, id;

-- name: ListRelationVersionsForTarget :many
SELECT * FROM relation_versions
WHERE target_object_version_id = @object_version_id
ORDER BY created_at, id;
