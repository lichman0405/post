-- Manifest export snapshot (task T0206). The manifest of a state is the
-- project's complete research-state graph as of that state (docs/07 §1):
-- every scientific object version and relation version whose state_id is
-- the state itself or one of its ancestors on the per-branch state chain
-- (walked here by recursive CTE over project_states.parent_state_id), plus
-- the blob attachments whose OWN state_id (00035) AND owning version are
-- in the lineage. A version created on a forked branch is not part of the
-- ancestor branch's snapshot, and vice versa.
--
-- No ORDER BY here on purpose: the canonical ordering of the manifest
-- arrays is the manifest package's rule (internal/rsg/manifest.Build), not
-- the store's — the export must not depend on which index the planner
-- walked.

-- name: ListManifestObjectVersions :many
WITH RECURSIVE lineage(id) AS (
  SELECT project_states.id FROM project_states WHERE project_states.id = @state_id
  UNION
  SELECT ps.parent_state_id FROM project_states ps
  JOIN lineage l ON ps.id = l.id
  WHERE ps.parent_state_id IS NOT NULL
)
SELECT sov.*, so.object_type
FROM scientific_object_versions sov
JOIN scientific_objects so ON so.id = sov.object_id
WHERE sov.state_id IN (SELECT id FROM lineage);

-- name: ListManifestRelationVersions :many
-- Relation versions whose state_id is in the lineage — the same recursive
-- ancestor walk the object-version query uses (a version created on a
-- forked branch is not part of the ancestor branch's snapshot).
WITH RECURSIVE lineage(id) AS (
  SELECT project_states.id FROM project_states WHERE project_states.id = @state_id
  UNION
  SELECT ps.parent_state_id FROM project_states ps
  JOIN lineage l ON ps.id = l.id
  WHERE ps.parent_state_id IS NOT NULL
)
SELECT * FROM relation_versions
WHERE state_id IN (SELECT id FROM lineage);

-- name: ListManifestBlobRefs :many
-- One row per attached blob (a blob attached to several object versions
-- still appears once: the manifest pins the blob, not the attachment
-- cardinality — attachment roles live in blob_attachments). The filter is
-- the ATTACHMENT's own state (its creating state, 00035), never the
-- owning version's — an attachment created in a later state must not leak
-- into earlier states' manifests (a state's hash is a pure function of
-- the state's own recorded content) — AND the owning version's own
-- lineage membership, so every blob ref names a version present in the
-- same manifest (an attachment pointing at a version outside the lineage
-- never renders a dangling ref).
WITH RECURSIVE lineage(id) AS (
  SELECT project_states.id FROM project_states WHERE project_states.id = @state_id
  UNION
  SELECT ps.parent_state_id FROM project_states ps
  JOIN lineage l ON ps.id = l.id
  WHERE ps.parent_state_id IS NOT NULL
)
SELECT DISTINCT b.id, b.content_hash
FROM blobs b
JOIN blob_attachments ba ON ba.blob_id = b.id
JOIN scientific_object_versions sov ON sov.id = ba.scientific_object_version_id
WHERE ba.state_id IN (SELECT id FROM lineage)
  AND sov.state_id IN (SELECT id FROM lineage);
