-- Asset metadata revision (T0706): the one write of the metadata surface,
-- and the reads it makes under its own lock.
--
-- # What the update is, and what it deliberately is not
--
-- docs/11 §4 (RELEASE_ASSET_HUB.md:21) makes an asset's metadata
-- independently revisable, keeps the audit, and forbids the revision from
-- producing a new scientific version. So the write is an IN-PLACE update
-- of research_assets — the mutable asset row — and there is no second
-- table, no revision row and no version row anywhere in this file. The
-- audit half is the audit_log row the same transaction appends
-- (persistence.AssetMetadataStore.ReviseMetadata); the "no new version"
-- half is 00014's own asymmetry: research_assets carries no append-only
-- row trigger, so this UPDATE is legal, while research_asset_versions
-- does (00014:59-61), so the version stream cannot be written in place
-- even by mistake.
--
-- The metadata columns are migration 00125's. title and slug are not new
-- — they are the asset's display fields since 00010 — and they are
-- revised HERE rather than by a publish, because
-- internal/application/assetpublish refuses title and slug for a version
-- published under an existing asset ("publishing a version is not an
-- asset-metadata revision", command.go:197).
--
-- cover_blob_id is NOT in the SET list and must not be added to it. It is
-- the reserved cover slot (00125); nothing in this build serves blob bytes
-- (no upload route, no download route, no signed URL, no TTL), so the
-- revision command refuses a cover change by name rather than writing a
-- cover no reader could fetch. A later task that builds the blob transfer
-- path adds cover_blob_id here, in a query of its own or in this one, with
-- the serving route it needs.

-- name: ReviseResearchAssetMetadata :one
-- One metadata revision: the six revisable columns, written with their
-- FINAL values. The caller resolves "nil means unchanged" against the row
-- it locked first — GetResearchAssetByPIDForUpdate, declared in
-- asset_governance.sql because T0711's rights-holder chain locks the same
-- row for its own append, and reused rather than redeclared here (two
-- statements that lock one row for one reason should be one statement) —
-- and passes the result in, so this statement never has to spell a
-- COALESCE. The lock is what makes the before-values and this UPDATE
-- atomic with respect to every other writer of the row: a concurrent
-- revision, publish or transfer waits, reads the first's values as its
-- before, and writes its own.
--
-- The column list is the whole SET, explicitly, rather than
-- `SET (title, slug, ...) = (SELECT ...)`: a parameter per column keeps
-- the generated params struct one field per column, so a future column
-- added to 00125's set cannot be picked up by a wildcard and written from
-- a zero value.
UPDATE research_assets
SET title = @title,
    slug = @slug,
    description = @description,
    keywords = @keywords,
    contact = @contact,
    documentation = @documentation
WHERE id = @id
RETURNING *;
