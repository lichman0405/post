-- RSG query surface (T0209): as-of version selection and the traversal's
-- adjacency reads. All shapes are rebuildable from the canonical append-only
-- history; nothing here adds semantic content (CLAUDE.md §7: RSG graph
-- relations run on the relation tables / recursive CTE).
--
-- The state-lineage walk (ListStateLineage) lives in rsg_query_store.go as a
-- raw pgx query: the sqlc analyzer (v1.31.1) cannot resolve the recursive
-- CTE's self-reference and rejects valid PostgreSQL ("column reference id is
-- ambiguous"), and the walk is one bounded query the adapter owns wholesale.

-- name: ListObjectVersionsAsOf :many
-- Each object of the project with its as-of version: the newest version whose
-- state is in the lineage (nil lineage = no state pinning, the newest version
-- overall). object_types nil = every type. The version log is append-only and
-- version_no is monotonically increasing, so the newest version_no in the
-- lineage is exactly the version the object had reached at the pinned state.
SELECT DISTINCT ON (so.id) so.*, sov.*
FROM scientific_objects so
JOIN scientific_object_versions sov ON sov.object_id = so.id
WHERE so.project_id = @project_id
  AND (@object_types::text[] IS NULL OR so.object_type = ANY(@object_types))
  AND (@lineage::uuid[] IS NULL OR sov.state_id = ANY(@lineage))
ORDER BY so.id, sov.version_no DESC;

-- name: ListRelationVersionsAsOf :many
-- Each relation of the project with its as-of version (same lineage rule as
-- objects) plus the endpoint objects' context (object id, object type,
-- project) the query selection rules need. relation_types nil = every type.
SELECT DISTINCT ON (r.id)
  r.id AS relation_id,
  r.project_id AS relation_project_id,
  rv.id AS relation_version_id,
  rv.version_no,
  rv.state_id,
  rv.relation_type,
  rv.source_object_version_id,
  rv.target_object_version_id,
  rv.payload,
  rv.integrity_hash,
  rv.created_by,
  rv.created_at,
  sov_s.object_id AS source_object_id,
  so_s.object_type AS source_object_type,
  so_s.project_id AS source_project_id,
  sov_t.object_id AS target_object_id,
  so_t.object_type AS target_object_type,
  so_t.project_id AS target_project_id
FROM relations r
JOIN relation_versions rv ON rv.relation_id = r.id
JOIN scientific_object_versions sov_s ON sov_s.id = rv.source_object_version_id
JOIN scientific_objects so_s ON so_s.id = sov_s.object_id
JOIN scientific_object_versions sov_t ON sov_t.id = rv.target_object_version_id
JOIN scientific_objects so_t ON so_t.id = sov_t.object_id
WHERE r.project_id = @project_id
  AND (@relation_types::text[] IS NULL OR rv.relation_type = ANY(@relation_types))
  AND (@lineage::uuid[] IS NULL OR rv.state_id = ANY(@lineage))
ORDER BY r.id, rv.version_no DESC;

-- name: ListAdjacentRelationVersions :many
-- The traversal hop: every relation version whose source or target is one
-- of @version_ids, at its as-of version (lineage rule as above), with both
-- endpoints' object context so the caller can tell which side was the
-- frontier. NOT project-filtered on purpose: a traversal hop may touch a
-- relation of another project (lineage edges across projects), and the
-- service authorizes every hop's projects before the row can enter the
-- result — the query returns the row so the authorization can see it,
-- never the other way round.
SELECT DISTINCT ON (rv.relation_id)
  r.id AS relation_id,
  r.project_id AS relation_project_id,
  rv.id AS relation_version_id,
  rv.version_no,
  rv.state_id,
  rv.relation_type,
  rv.source_object_version_id,
  rv.target_object_version_id,
  rv.payload,
  rv.integrity_hash,
  rv.created_by,
  rv.created_at,
  sov_s.object_id AS source_object_id,
  so_s.object_type AS source_object_type,
  so_s.project_id AS source_project_id,
  sov_t.object_id AS target_object_id,
  so_t.object_type AS target_object_type,
  so_t.project_id AS target_project_id
FROM relation_versions rv
JOIN relations r ON r.id = rv.relation_id
JOIN scientific_object_versions sov_s ON sov_s.id = rv.source_object_version_id
JOIN scientific_objects so_s ON so_s.id = sov_s.object_id
JOIN scientific_object_versions sov_t ON sov_t.id = rv.target_object_version_id
JOIN scientific_objects so_t ON so_t.id = sov_t.object_id
WHERE (rv.source_object_version_id = ANY(@version_ids::uuid[]) OR rv.target_object_version_id = ANY(@version_ids::uuid[]))
  AND (@lineage::uuid[] IS NULL OR rv.state_id = ANY(@lineage))
ORDER BY rv.relation_id, rv.version_no DESC;

-- name: ListObjectVersionsByIDs :many
-- Batch fetch for the traversal: the pinned object + version rows of the
-- version ids collected by a BFS level. Unfiltered by project on purpose —
-- the caller passes only version ids whose projects already passed the
-- per-hop authorization.
SELECT so.*, sov.*
FROM scientific_object_versions sov
JOIN scientific_objects so ON so.id = sov.object_id
WHERE sov.id = ANY(@version_ids::uuid[])
ORDER BY sov.created_at, sov.id;
