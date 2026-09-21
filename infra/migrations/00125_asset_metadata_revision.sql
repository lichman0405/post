-- +goose Up
-- T0706. Asset metadata revision: the mutable, asset-level metadata of
-- docs/11 §4, and the five columns that hold it.
--
-- docs/11 §4 (RELEASE_ASSET_HUB.md:21) is the whole task:
--
--   Scientific Version 不可变；描述、keywords、cover、contact、
--   documentation 等 Asset Metadata 可独立 revision，保留 audit，
--   不产生新的 scientific version。
--
-- Read literally, that sentence asks for three things and forbids one. A
-- version is immutable; metadata is revisable; the revision is audited;
-- and the revision does NOT produce a new scientific version. So the
-- revision is an IN-PLACE update of the asset row, and this migration is
-- the columns that update writes — deliberately not a version table, not
-- a revision-history table, and not a second row per revision. "Asset
-- metadata" and "scientific version" are two different things in docs/11
-- precisely so that revising one cannot produce the other, and a
-- revision-history table would rebuild the version stream docs/11 §4
-- just said must not exist (its audit is the audit_log row the same
-- transaction writes — 00012's before_summary/after_summary columns are
-- what metadata revisions record into).
--
-- # Why research_assets, and why these columns are mutable there
--
-- A research version's metadata lives in its immutable manifest document
-- (research_asset_versions.manifest, 00010; the asset type's required
-- metadata table is internal/assets.RequiredMetadata). That document
-- belongs to the VERSION: it is covered by the version's integrity hash
-- and its row is append-only (00014). What docs/11 §4 makes revisable is
-- the layer above it — the asset's own descriptive metadata — and
-- research_assets is that row (00010, extended by 00064 with the pid).
-- It carries no append-only row trigger, so an in-place update is a legal
-- write here and an illegal one on the version row; that asymmetry is the
-- schema expressing the same boundary the code does
-- (internal/application/assetpublish/command.go:197 refuses title/slug
-- when publishing a version of an existing asset, because "publishing a
-- version is not an asset-metadata revision").
--
-- # What is NOT here
--
-- No pid, no asset_type and no origin_project_id: those are the asset's
-- IDENTITY, not its metadata, and docs/11 §2 makes the pid stable across
-- renames and transfers. slug is metadata, and the reason renaming it is
-- safe is 00064's: the pid — and therefore every URL built from it — is
-- random and never derived from the slug.
--
-- # The columns, one per metadata item of docs/11 §4
--
-- description, keywords, contact and documentation are the four items the
-- sentence lists that this surface writes. Each is NOT NULL with an empty
-- default, which is the shape the asset already had: a pre-T0706 row has
-- no description, and "" states that, while NULL would be the absence of
-- a statement. text[] for the three list-shaped items because the values
-- are plain strings (a keyword, a contact, a documentation link) and the
-- column then reads back as the list it was declared as; a jsonb document
-- would be a shape the platform invented where a list is what is stored.
-- There is deliberately no CHECK on their contents beyond the array
-- itself: internal/assets bounds their length and shape on the
-- application path, the same division the manifest's metadata block uses
-- (00067 makes it a JSON object at the storage layer; the field tables
-- live in Go).
--
-- cover_blob_id is the fifth item, and it is RESERVED rather than served.
-- docs/11 §4 lists cover among the revisable metadata, and docs/11 §3
-- puts a cover in the publish checklist's required metadata, so the
-- column has to exist for the item to be representable at all. What does
-- not exist is any way to fetch the bytes: the blob surface today is
-- internal/storage's port declaration and the blobs/blob_attachments
-- tables (00008) — there is no upload route, no download route, and no
-- signed-URL/TTL mechanism anywhere in the tree, so a cover that could be
-- SET would be an image no reader can ever see. The column is therefore
-- declared, nullable, foreign-keyed to the row it would name, and NOT
-- written by any code path in this build: the revision command refuses a
-- cover change by name instead of silently dropping it
-- (internal/application/assetmetadata.ErrCoverNotSupported), and the
-- blob-side channel that would make it real belongs to the task that
-- builds the blob transfer path. Null means "no cover recorded", which is
-- the truth for every asset today.
ALTER TABLE research_assets
  ADD COLUMN description text NOT NULL DEFAULT '',
  ADD COLUMN keywords text[] NOT NULL DEFAULT '{}',
  ADD COLUMN contact text[] NOT NULL DEFAULT '{}',
  ADD COLUMN documentation text[] NOT NULL DEFAULT '{}',
  ADD COLUMN cover_blob_id uuid REFERENCES blobs(id) ON DELETE RESTRICT;

COMMENT ON COLUMN research_assets.description IS
  'T0706. The asset''s description — revisable asset metadata (docs/11 §4), NOT part of any scientific version. Revised in place by internal/application/assetmetadata, in the same transaction as the audit_log row recording the revision. Empty string means "no description recorded"; NULL is never stored.';

COMMENT ON COLUMN research_assets.keywords IS
  'T0706. The asset''s keyword list — revisable asset metadata (docs/11 §4). An array because it is stored as a list; the count/length bounds live in internal/assets (MaxKeywords, MaxKeywordLen) and are enforced on the write path, not as a CHECK here.';

COMMENT ON COLUMN research_assets.contact IS
  'T0706. How to reach whoever is responsible for the asset — revisable asset metadata (docs/11 §4). Plain strings, not a structured party reference: docs/11 §6 already gives the responsible parties their own roles (Rights Holder, Custodian, Maintainer, Creator, Contributor) in their own tables, and a second structured copy here would be a second answer to "who is responsible".';

COMMENT ON COLUMN research_assets.documentation IS
  'T0706. Where the asset is documented — revisable asset metadata (docs/11 §4). Plain strings: this build stores the reference the publisher declared and does not dereference it, so no URL shape is asserted here (a DOI, a repository path and an https link are all references).';

COMMENT ON COLUMN research_assets.cover_blob_id IS
  'T0706. The blob that would be the asset''s cover — RESERVED, and written by nothing in this build. docs/11 §4 lists cover among the revisable metadata, so the slot exists; the blob surface has no upload/download route and no signed-URL/TTL mechanism, so a cover set today is an image no reader could fetch. The revision command refuses a cover change by name (assetmetadata.ErrCoverNotSupported) rather than dropping it, and the channel that would fill this column belongs to the blob-transfer task. NULL is the state of every asset today.';
