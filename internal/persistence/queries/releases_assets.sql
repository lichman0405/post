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
