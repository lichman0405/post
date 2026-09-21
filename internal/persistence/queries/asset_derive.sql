-- Research asset fork/derive (T0708): the writes and the ledger reads of
-- the derive command, plus the one read the parent resolution needs.
--
-- The derivation is the second governed write over research_assets /
-- research_asset_versions (the first is the publish, asset_publish.sql).
-- What it adds to the publish is the two things docs/11 §5 names as its
-- whole content — a NEW identity and the lineage edge that records where
-- it came from — so the rows it writes are the same three the publish
-- writes (research_assets, research_asset_versions, the audit row), plus
-- the asset_lineage edge and the derivation's own ledger row.
--
-- Every write below happens inside ONE transaction
-- (persistence.AssetDeriveStore.Derive) together with the parent
-- resolution that authorized it, the rights verdict over the parent's
-- stored declaration, the impact re-check, the audit row and the domain
-- event: a refused derivation writes nothing.
--
-- # No UPDATE and no DELETE, anywhere in this file
--
-- research_asset_versions and asset_lineage carry the 00014 append-only
-- row triggers, and this file never gives them anything to refuse: the
-- parent's version row is READ, its columns are not touched, and the
-- child's edge is INSERTed. The parent is immutable in this path because
-- nothing here writes to it — not because a trigger would have stopped it.

-- name: GetDeriveParentVersion :one
-- One published version named by its asset's pid and its own label: the
-- version a derivation is FROM. The pid@version pair is the canonical way
-- a version is named on the wire (internal/assets.NewParentVersionRef),
-- and it resolves here to the version ROW id the lineage edge is keyed
-- by — never to the asset id, and never to the slug (which is mutable
-- display metadata, internal/assets/asset.go).
--
-- What comes back beside the row id is what the two decisions over it
-- need, and nothing is filtered here:
--
--   - rights_json, whole, because the verdict over it is internal/rights'
--     and this query must not pre-judge it (an unreadable declaration is
--     a refusal the command makes by name, not a row this read drops).
--   - the origin project's id and visibility, because whether the caller
--     may derive from this version at all is the asset page's read gate:
--     a public project, or a caller who is a member of it. The membership
--     half is not answered here — it is asked of the same project service
--     every other membership question goes through (one implementation of
--     "is this caller a member") — and the visibility half comes back even
--     when the project is private, because "there is no such version" and
--     "there is one you may not read" must be the same answer, which the
--     model can only make if it is told both.
--
-- The joins are INNER on NOT NULL foreign keys and on the asset's own
-- project: a version row always has exactly one asset row and one origin
-- project, so this query drops nothing a caller could have been told
-- about. A pid@version pair with no row answers pgx.ErrNoRows, which the
-- store turns into the same not-found the unreadable case is answered
-- with.

SELECT rav.id AS version_id,
       rav.version,
       rav.visibility,
       rav.rights_json,
       rav.asset_id,
       ra.pid AS asset_pid,
       ra.title AS asset_title,
       ra.asset_type,
       ra.origin_project_id::text AS origin_project_id,
       p.visibility AS project_visibility
FROM research_asset_versions rav
JOIN research_assets ra ON ra.id = rav.asset_id
JOIN projects p ON p.id = ra.origin_project_id
WHERE ra.pid = @pid AND rav.version = @version;

-- name: GetDerivedAsset :one
-- The derivation an Idempotency-Key already created, in full: the child
-- version row with its asset's pid, and the edge — the PARENT's pid and
-- label and the relation — the ledger row recorded.
--
-- The read is driven by the ledger row rather than by the child version,
-- and that is deliberate: a version may carry more than one lineage edge
-- over its life (asset_lineage's primary key is the triple, and a later
-- lifecycle action may add a 'supersedes' edge between the same two
-- ends), so a read keyed on the child would be a read that can return
-- several rows for one key. The ledger names exactly the edge THIS
-- derivation wrote, which is what a replay has to answer with.
--
-- UNIQUE(project_id, idempotency_key) makes this at most one row, and
-- pgx.ErrNoRows (no row) is what the store reads as "a new key".

SELECT rav.id,
       rav.asset_id,
       ca.pid AS asset_pid,
       rav.version,
       rav.manifest,
       rav.rights_json,
       rav.visibility,
       rav.integrity_hash,
       rav.published_by,
       rav.published_at,
       rav.origin_refs,
       pa.pid AS parent_asset_pid,
       parv.version AS parent_version,
       adc.relation_type
FROM asset_derive_creations adc
JOIN research_asset_versions rav ON rav.id = adc.asset_version_id
JOIN research_assets ca ON ca.id = rav.asset_id
JOIN research_asset_versions parv ON parv.id = adc.parent_asset_version_id
JOIN research_assets pa ON pa.id = parv.asset_id
WHERE adc.project_id = @project_id AND adc.idempotency_key = @idempotency_key;

-- name: CreateAssetDeriveCreation :one
-- The ledger row, written in the same transaction as the child version and
-- the edge it names: a derivation that rolled back leaves no key behind,
-- and a key that committed names a child version and a parent edge that
-- both exist.
--
-- relation_type is one of the two values a derivation writes (the narrower
-- set migration 00128 checks on this table, which excludes the
-- asset_lineage CHECK's third value 'supersedes'): the ledger records what
-- this command did, and this command never supersedes.

INSERT INTO asset_derive_creations
  (project_id, idempotency_key, asset_version_id, parent_asset_version_id, relation_type)
VALUES
  (@project_id, @idempotency_key, @asset_version_id, @parent_asset_version_id, @relation_type)
RETURNING *;

-- name: CreateAssetLineage :one
-- The lineage edge itself: the whole reason this command exists
-- (docs/11 §5 "保留 lineage"; docs/31 Gate C "Asset
-- PID/version/lineage/rights/reference/dependency/fork 工作").
--
-- Both ends are VERSION ROW ids — the parent's is the id the resolution
-- above read, the child's is the id the insert just assigned — because
-- that is what asset_lineage is keyed by (00010). The edge names an exact
-- immutable version, not an asset and not a slug: an asset accumulates
-- versions and its slug can change, so either would make "forked from
-- WHAT" a question the edge cannot answer later.
--
-- An INSERT, and only ever an INSERT. asset_lineage carries the 00014
-- append-only trigger; this path does not rely on it to stay honest, it
-- simply has nothing to update or delete: a derivation records a fact
-- that happened and never revises it.

INSERT INTO asset_lineage (parent_asset_version_id, child_asset_version_id, relation_type)
VALUES (@parent_asset_version_id, @child_asset_version_id, @relation_type)
RETURNING *;
