-- Releases, research assets, publications (canonical tables:
-- validation_results, releases, research_assets, research_asset_versions,
-- knowledge_publications). Release/asset versions are immutable (invariant 5).

-- name: RecordValidationResult :one
INSERT INTO validation_results (project_id, state_id, gate, status, result_json)
VALUES (@project_id, @state_id, @gate, @status, @result_json)
RETURNING *;

-- name: CreateRelease :one
INSERT INTO releases (project_id, version, title, state_id, policy_version_id, org_policy_version_id, manifest, manifest_hash, created_by)
VALUES (@project_id, @version, @title, @state_id, @policy_version_id, @org_policy_version_id, @manifest, @manifest_hash, @created_by)
RETURNING *;

-- name: GetReleaseByProjectAndVersion :one
SELECT * FROM releases
WHERE project_id = @project_id AND version = @version;

-- name: ListReleases :many
SELECT * FROM releases
WHERE project_id = @project_id
ORDER BY created_at DESC, id DESC;

-- name: GetRelease :one
SELECT * FROM releases
WHERE project_id = @project_id AND id = @id;

-- name: GetReleaseCreation :one
SELECT release_id FROM release_creations
WHERE project_id = @project_id AND idempotency_key = @idempotency_key;

-- name: CreateReleaseCreation :one
INSERT INTO release_creations (project_id, idempotency_key, release_id)
VALUES (@project_id, @idempotency_key, @release_id)
RETURNING *;

-- name: CreateResearchAsset :one
INSERT INTO research_assets (asset_type, slug, title, origin_project_id)
VALUES (@asset_type, @slug, @title, @origin_project_id)
RETURNING *;

-- name: PublishResearchAssetVersion :one
-- The publish command's version insert (T0705). origin_refs is written
-- here because it is NOT NULL with two CHECKs since 00064 (at least one
-- element, no NULL element) and the version's provenance is exactly what
-- that column is: a publish that left it to a default could not store a
-- row at all. This query had no producer before T0705 — the preview
-- (T0704) only reads — so extending it is not a change to a shipped
-- writer; issue #225 recorded the gap when the column was added.
INSERT INTO research_asset_versions
    (asset_id, version, source_release_id, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
VALUES
    (@asset_id, @version, @source_release_id, @manifest, @rights_json, @visibility, @integrity_hash, @published_by, @origin_refs)
RETURNING *;

-- name: GetResearchAssetVersion :one
SELECT * FROM research_asset_versions
WHERE asset_id = @asset_id AND version = @version;

-- name: PublishKnowledgePublication :one
-- The publish command's insert (T0805). This query had no producer before
-- it — knowledge_publications has had no writer since migration 00010 —
-- so extending it with the pid column (migration 00083) is not a change
-- to a shipped writer, the way PublishResearchAssetVersion's origin_refs
-- was not one for T0705.
--
-- The pid is passed in and never left to the column DEFAULT: migration
-- 00064 records the same decision for assets ("the application-generated
-- path ... arrives with T0705 (publish)"), and a persistent identifier
-- that two writers derive differently is not a persistent identity. The
-- DEFAULT stays for rows written before the publish command could
-- generate one.
INSERT INTO knowledge_publications (object_version_id, public_version, rights_json, published_by, pid)
VALUES (@object_version_id, @public_version, @rights_json, @published_by, @pid)
RETURNING *;

-- name: ListReleaseReviews :many
-- The review/approval record of one release (T0605): every review row of
-- the research PRs targeting main whose proposed state is the released
-- state or one of its ancestors — the reviews that accepted this lineage
-- into main (docs/09 §4: frozen main updates only through PR merge, so a
-- proposed state inside main's lineage got there through its PR). The
-- target filter names main explicitly: a duplicate proposal of the same
-- state against another branch is not part of main's acceptance record.
-- Ordered by PR number then review time then row id (a total order — the
-- release manifest's canonical sorting is the releases package's rule,
-- not the store's).
WITH RECURSIVE lineage(id) AS (
  SELECT project_states.id FROM project_states WHERE project_states.id = @state_id
  UNION
  SELECT ps.parent_state_id FROM project_states ps
  JOIN lineage l ON ps.id = l.id
  WHERE ps.parent_state_id IS NOT NULL
)
SELECT pr.number AS pull_request_number,
       pr.proposed_state_id,
       r.id,
       r.reviewer_id,
       r.review_kind,
       r.decision,
       r.body,
       r.created_at
FROM reviews r
JOIN pull_requests pr ON pr.id = r.pull_request_id
WHERE pr.target_branch_id = @main_branch_id
  AND pr.proposed_state_id IN (SELECT id FROM lineage)
ORDER BY pr.number, r.created_at, r.id;
