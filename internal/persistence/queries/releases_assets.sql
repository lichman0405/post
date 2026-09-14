-- Releases, research assets, publications (canonical tables:
-- validation_results, releases, research_assets, research_asset_versions,
-- knowledge_publications). Release/asset versions are immutable (invariant 5).

-- name: RecordValidationResult :one
INSERT INTO validation_results (project_id, state_id, gate, status, result_json)
VALUES (@project_id, @state_id, @gate, @status, @result_json)
RETURNING *;

-- name: CreateRelease :one
INSERT INTO releases (project_id, version, title, state_id, policy_version_id, manifest, manifest_hash, created_by)
VALUES (@project_id, @version, @title, @state_id, @policy_version_id, @manifest, @manifest_hash, @created_by)
RETURNING *;

-- name: GetReleaseByProjectAndVersion :one
SELECT * FROM releases
WHERE project_id = @project_id AND version = @version;

-- name: CreateResearchAsset :one
INSERT INTO research_assets (asset_type, slug, title, origin_project_id)
VALUES (@asset_type, @slug, @title, @origin_project_id)
RETURNING *;

-- name: PublishResearchAssetVersion :one
INSERT INTO research_asset_versions
    (asset_id, version, source_release_id, manifest, rights_json, visibility, integrity_hash, published_by)
VALUES
    (@asset_id, @version, @source_release_id, @manifest, @rights_json, @visibility, @integrity_hash, @published_by)
RETURNING *;

-- name: GetResearchAssetVersion :one
SELECT * FROM research_asset_versions
WHERE asset_id = @asset_id AND version = @version;

-- name: PublishKnowledgePublication :one
INSERT INTO knowledge_publications (object_version_id, public_version, rights_json, published_by)
VALUES (@object_version_id, @public_version, @rights_json, @published_by)
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
