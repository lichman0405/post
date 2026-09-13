-- Typed relations and their append-only version log (canonical tables:
-- relations, relation_versions). Historical content is never UPDATEd in
-- place; a new version row is inserted instead (docs/53). Every version
-- pins its endpoints to exact scientific object versions (docs/07 §3).

-- name: CreateRelation :one
INSERT INTO relations (project_id)
VALUES (@project_id)
RETURNING *;

-- name: GetRelationByID :one
SELECT * FROM relations WHERE id = @id;

-- name: BumpRelationVersionNo :one
-- The expected_version compare-and-swap (T0203): advance the head pointer
-- from @expected_version_no to @expected_version_no + 1, but only while it
-- still equals @expected_version_no. Zero rows returned means the relation
-- does not exist or the expectation lost a race — the caller distinguishes
-- the two and reports EXPECTED_VERSION_MISMATCH (docs/45) either way.
UPDATE relations
   SET current_version_no = current_version_no + 1
 WHERE id = @relation_id AND current_version_no = @expected_version_no
RETURNING current_version_no;

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

-- name: CanonicalizeRelationPayload :one
-- jsonb normalizes JSON on input (key order, whitespace). The repository
-- stores that canonical form, and the integrity hash is the sha256 of the
-- canonical text, so a read payload always re-hashes to its stored hash.
SELECT @payload::jsonb AS payload;

-- name: GetRelationVersionByNo :one
SELECT * FROM relation_versions
WHERE relation_id = @relation_id AND version_no = @version_no;

-- name: GetLatestRelationVersion :one
SELECT * FROM relation_versions
WHERE relation_id = @relation_id
ORDER BY version_no DESC
LIMIT 1;

-- name: ListRelationVersions :many
SELECT * FROM relation_versions
WHERE relation_id = @relation_id
ORDER BY version_no;

-- name: ListRelationVersionsByType :many
-- All versions of one relation type inside one project (the project
-- boundary rides on the relations container row).
SELECT rv.*
  FROM relation_versions rv
  JOIN relations r ON r.id = rv.relation_id
 WHERE r.project_id = @project_id AND rv.relation_type = @relation_type
 ORDER BY rv.created_at, rv.id;

-- name: ListRelationVersionsByTypes :many
-- The category query: callers expand a catalog category (dependency,
-- provenance, ...) to its type names.
SELECT rv.*
  FROM relation_versions rv
  JOIN relations r ON r.id = rv.relation_id
 WHERE r.project_id = @project_id AND rv.relation_type = ANY(@relation_types::text[])
 ORDER BY rv.created_at, rv.id;
