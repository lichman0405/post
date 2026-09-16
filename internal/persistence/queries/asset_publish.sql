-- Research asset publish governance (T0705): the writes and the ledger
-- reads of the publish command, plus the one read a replay needs.
--
-- The publish is the platform's highest-risk operation (docs/23 §4): it
-- widens a private artifact into the network's view and its result is an
-- immutable version row. Every write below happens inside ONE transaction
-- (persistence.AssetPublishStore.Publish) together with the impact
-- re-check that authorized it, the audit row and the domain event, so a
-- refused publish writes nothing and an accepted one is one unit.
--
-- The reads the impact preview is re-run over are NOT here: they are the
-- preview's own canonical queries (asset_preview.sql), reused verbatim so
-- that the preview a human saw and the preview the publish re-runs are
-- answered by one SQL definition. What this file adds is the ledger
-- (asset_publish_creations, migration 00072) and the two rows a publish
-- writes.

-- name: CreateResearchAssetWithPID :one
-- A publish that names no existing asset creates one, and the pid it is
-- created with comes from the application (assets.NewPID, migration
-- 00064's note: "the application-generated path ... arrives with T0705
-- (publish)"): the column DEFAULT exists for rows written before the
-- publish command could generate one, and a persistent identifier that
-- two writers derive differently is not a persistent identity.
INSERT INTO research_assets (asset_type, slug, title, origin_project_id, pid)
VALUES (@asset_type, @slug, @title, @origin_project_id, @pid)
RETURNING *;

-- name: GetResearchAssetByPIDRow :one
-- The asset row behind a pid, in full. GetPreviewAssetByPID (the
-- preview's own read) answers the three fields the preview checks; the
-- publish also needs the row's id to write the version's foreign key and
-- its slug to tell a create from a continuation.
SELECT * FROM research_assets WHERE pid = @pid;

-- name: GetAssetPublishCreation :one
-- The publish ledger lookup: the version an Idempotency-Key already
-- created, or no row (pgx.ErrNoRows) when the key is new. UNIQUE(project
-- _id, idempotency_key) makes this at most one row by construction.
SELECT asset_version_id FROM asset_publish_creations
WHERE project_id = @project_id AND idempotency_key = @idempotency_key;

-- name: CreateAssetPublishCreation :one
-- The ledger row, written in the same transaction as the version it
-- names: a publish that rolled back leaves no key behind, and a key that
-- committed names a version that exists.
INSERT INTO asset_publish_creations (project_id, idempotency_key, asset_version_id)
VALUES (@project_id, @idempotency_key, @asset_version_id)
RETURNING *;

-- name: GetPublishedAssetVersion :one
-- One stored version WITH the persistent identity of its asset. A replay
-- answers the version the key created, and the caller of a publish needs
-- the asset's pid to say which asset was published — it is not derivable
-- from the version row, whose asset_id is the internal uuid.
--
-- The join is an inner one on a NOT NULL foreign key: every version row
-- has exactly one asset row.
SELECT rav.id,
       rav.asset_id,
       ra.pid AS asset_pid,
       rav.version,
       rav.source_release_id,
       rav.manifest,
       rav.rights_json,
       rav.visibility,
       rav.integrity_hash,
       rav.published_by,
       rav.published_at,
       rav.origin_refs
FROM research_asset_versions rav
JOIN research_assets ra ON ra.id = rav.asset_id
WHERE rav.id = @id;
