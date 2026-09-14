-- Typed relations and their append-only version log (canonical tables:
-- relations, relation_versions). Historical content is never UPDATEd in
-- place; a new version row is inserted instead (docs/53). Every version
-- pins its endpoints to exact scientific object versions (docs/07 §3).

-- name: CreateRelation :one
INSERT INTO relations (project_id)
VALUES (@project_id)
RETURNING *;

-- name: CreateRelationWithID :one
-- The explicit-id variant (T0208): the consuming API service pre-generates
-- the relation id so the state commit's operation summary can name the real
-- entity (commit_linkage checks EntityID + version_no against the row).
INSERT INTO relations (id, project_id)
VALUES (@id, @project_id)
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

-- name: ListRelationVersionsForObject :many
-- The object-detail relations tab (T0210): every relation version whose
-- source or target endpoint pins a version of @object_id, newest first.
-- The endpoint joins resolve the display labels (type + title) in the same
-- round trip, so the page needs no N+1 lookups; the project boundary is
-- enforced on the relations container row (the endpoints' own project is
-- guaranteed equal by the relation write command).
SELECT rv.*,
       sv_src.object_id   AS source_object_id,
       src.object_type AS source_object_type,
       sv_src.title    AS source_title,
       sv_tgt.object_id   AS target_object_id,
       tgt.object_type AS target_object_type,
       sv_tgt.title    AS target_title
  FROM relation_versions rv
  JOIN relations r ON r.id = rv.relation_id
  JOIN scientific_object_versions sv_src ON sv_src.id = rv.source_object_version_id
  JOIN scientific_objects src ON src.id = sv_src.object_id
  JOIN scientific_object_versions sv_tgt ON sv_tgt.id = rv.target_object_version_id
  JOIN scientific_objects tgt ON tgt.id = sv_tgt.object_id
 WHERE r.project_id = @project_id
   AND (src.id = @object_id OR tgt.id = @object_id)
 ORDER BY rv.created_at DESC, rv.id DESC;
