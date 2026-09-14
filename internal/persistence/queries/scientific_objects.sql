-- Scientific objects and their append-only version log (canonical tables:
-- scientific_objects, scientific_object_versions). Historical content is never
-- UPDATEd in place; a new version row is inserted instead (docs/53).

-- name: CreateScientificObject :one
INSERT INTO scientific_objects (project_id, object_type, created_by)
VALUES (@project_id, @object_type, @created_by)
RETURNING *;

-- name: CreateScientificObjectWithID :one
-- The explicit-id variant (T0208): the consuming API service pre-generates
-- the object id so the state commit's operation summary can name the real
-- entity (commit_linkage checks EntityID + version_no against the row).
INSERT INTO scientific_objects (id, project_id, object_type, created_by)
VALUES (@id, @project_id, @object_type, @created_by)
RETURNING *;

-- name: GetScientificObjectByID :one
SELECT * FROM scientific_objects WHERE id = @id;

-- name: BumpScientificObjectVersionNo :one
-- The expected_version compare-and-swap (T0202): advance the head pointer
-- from @expected_version_no to @expected_version_no + 1, but only while it
-- still equals @expected_version_no. Zero rows returned means the object
-- does not exist or the expectation lost a race — the caller distinguishes
-- the two and reports EXPECTED_VERSION_MISMATCH (docs/45) either way.
UPDATE scientific_objects
   SET current_version_no = current_version_no + 1
 WHERE id = @object_id AND current_version_no = @expected_version_no
RETURNING current_version_no;

-- name: CreateScientificObjectVersion :one
INSERT INTO scientific_object_versions
    (object_id, version_no, state_id, branch_id, schema_id, schema_version,
     title, lifecycle_state, payload, visibility_policy_id, integrity_hash, created_by)
VALUES
    (@object_id, @version_no, @state_id, @branch_id, @schema_id, @schema_version,
     @title, @lifecycle_state, @payload, @visibility_policy_id, @integrity_hash, @created_by)
RETURNING *;

-- name: CanonicalizeScientificObjectPayload :one
-- jsonb normalizes JSON on input (key order, whitespace). The repository
-- stores that canonical form, and the integrity hash is the sha256 of the
-- canonical text, so a read payload always re-hashes to its stored hash.
SELECT @payload::jsonb AS payload;

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
