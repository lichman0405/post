-- RSG query surface (T0209): as-of version selection and the traversal's
-- adjacency reads. All shapes are rebuildable from the canonical append-only
-- history; nothing here adds semantic content (CLAUDE.md §7: RSG graph
-- relations run on the relation tables / recursive CTE).
--
-- The state-lineage walk (ListStateLineage) lives in rsg_query_store.go as a
-- raw pgx query: the sqlc analyzer (v1.31.1) cannot resolve the recursive
-- CTE's self-reference and rejects valid PostgreSQL ("column reference id is
-- ambiguous"), and the walk is one bounded query the adapter owns wholesale.
--
-- ENUMERATION RULE (ADR-027, T0818). The two seed reads below enumerate a
-- project's RSG by what its STATE LINEAGE CARRIES, not by the container's
-- `project_id`:
--
--   * the carrier is the state (`sov.state_id` / `rv.state_id`), and the
--     project is the authorization surface — a version belongs to the RSG of
--     the project whose state holds it;
--   * `scientific_objects.project_id` / `relations.project_id` cannot answer
--     "what is in this project's RSG": they only happen to agree with the
--     state's project while a container never carries a version from another
--     project, which is exactly what a merged external fork breaks. The merge
--     materializes the accepted content onto the CONTRIBUTOR's containers
--     (ADR-027 Decision 1 — the landed version keeps its source identity, so
--     there is one truth and one version sequence per object), and those rows
--     are carried by the upstream project's states. Selecting by container
--     made the upstream project unable to read its own main back.
--
-- The project's state set is `project_states.project_id = @project_id`
-- (served by the existing UNIQUE(project_id, state_hash) index); with a pin,
-- `@lineage` is that state's ancestor chain, already project-verified by the
-- walk, so the two conditions agree there. No new index is required: the
-- pinned shape uses relation_versions_state_idx /
-- scientific_object_versions_state_idx (00026) and the unpinned one the
-- project_states unique index.
--
-- The same rule decides what a seed edge may DISCLOSE, so
-- ListRelationVersionsAsOf also reports, for both of its pins, the project
-- whose state carries the pinned version (`*_carried_by`, a project_states
-- lookup on the pinned version's state_id). The caller authorizes a seed edge
-- by its pins' carriers, never by the containers the rows wear: for landed
-- content those are the contributor's, and gating on them would hide the
-- project's own accepted content from it (the T0818 defect).

-- name: ListObjectVersionsAsOf :many
-- One row per object the project's lineage carries, at the newest version the
-- lineage carries (nil lineage = no state pinning, the newest version among
-- the project's states). object_types nil = every type. The version log is
-- append-only and version_no is monotonically increasing, so the newest
-- version_no in the lineage is exactly the version the object had reached at
-- the pinned state. The object row is the container the version HANGS ON: an
-- object the upstream project accepted from an external fork is the
-- contributor's container, reported as such.
SELECT DISTINCT ON (so.id) so.*, sov.*
FROM scientific_objects so
JOIN scientific_object_versions sov ON sov.object_id = so.id
WHERE (@object_types::text[] IS NULL OR so.object_type = ANY(@object_types))
  AND (@lineage::uuid[] IS NULL OR sov.state_id = ANY(@lineage))
  AND sov.state_id IN (SELECT ps.id FROM project_states ps WHERE ps.project_id = @project_id)
ORDER BY so.id, sov.version_no DESC;

-- name: ListRelationVersionsAsOf :many
-- Each relation the project's lineage carries with its as-of version (same
-- state-lineage rule as objects) plus the endpoint objects' context the query
-- selection rules need. relation_types nil = every type.
--
-- The endpoint context carries TWO facts about each pinned endpoint version,
-- and the reader needs both:
--   * `*_project_id` is the CONTAINER the version hangs on — after a merge
--     landed an external fork's proposal that container is the contributor's,
--     while the pin is a version the merge wrote upstream (Decision 1);
--   * `*_carried_by` is the project whose STATE CARRIES the pinned version —
--     the authorization surface of the pin (Decisions 2 and 4). The two are
--     equal for every version written in the project that owns its container,
--     and differ exactly for content a merge landed: there the pin's carrier
--     is the project reading this row, and its container is the contributor's.
-- `state_id` is NOT NULL with a RESTRICT foreign key (00005 line 15 / 00006
-- line 12), so the carrier join is total: every pin names exactly one.
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
  ps_s.project_id AS source_carried_by,
  sov_t.object_id AS target_object_id,
  so_t.object_type AS target_object_type,
  so_t.project_id AS target_project_id,
  ps_t.project_id AS target_carried_by
FROM relations r
JOIN relation_versions rv ON rv.relation_id = r.id
JOIN scientific_object_versions sov_s ON sov_s.id = rv.source_object_version_id
JOIN scientific_objects so_s ON so_s.id = sov_s.object_id
JOIN project_states ps_s ON ps_s.id = sov_s.state_id
JOIN scientific_object_versions sov_t ON sov_t.id = rv.target_object_version_id
JOIN scientific_objects so_t ON so_t.id = sov_t.object_id
JOIN project_states ps_t ON ps_t.id = sov_t.state_id
WHERE (@relation_types::text[] IS NULL OR rv.relation_type = ANY(@relation_types))
  AND (@lineage::uuid[] IS NULL OR rv.state_id = ANY(@lineage))
  AND rv.state_id IN (SELECT ps.id FROM project_states ps WHERE ps.project_id = @project_id)
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
