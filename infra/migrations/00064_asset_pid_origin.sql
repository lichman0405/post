-- +goose Up
-- Research asset persistent identity (T0701): every research asset gets
-- a pid — the persistent identifier its public URLs are built from —
-- and every published asset version pins its origin refs.
--
-- The pid is the asset's public identity, deliberately independent of
-- every mutable attribute: it is random, never derived from the slug,
-- the owning organization, or anything else that can be renamed or
-- transferred (docs/11 §2: 每个 Asset 有 persistent ID; acceptance:
-- asset ID 不随 slug/org 变化). A slug rename and an org transfer both
-- leave the pid — and therefore the persistent URL /assets/{pid} —
-- untouched. The format is the Go package's (internal/assets): 26
-- Crockford base32 characters (lowercase, URL-safe, no padding), the
-- same shape the CHECK below enforces. The DEFAULT is a random uuid's
-- first 26 hex characters — a Crockford subset — so every row the
-- database fills gets a valid, unique pid.
--
-- At this commit no writer supplies a pid: CreateResearchAsset
-- (internal/persistence/queries) inserts four columns and lets the
-- DEFAULT fill pid, and assets.NewPID() has no producer yet. The
-- application-generated path — a create/publish command calling
-- NewPID() — arrives with T0705 (publish); until then the DEFAULT is
-- both the backfill for rows that predate this migration and the source
-- of pids for new rows.
--
-- origin_refs on research_asset_versions is the canonical provenance pin
-- the version schema demands
-- (specs/schemas/research-asset-version.schema.json: origin_refs,
-- minItems 1): where this published version came from, as kind:value
-- refs (internal/assets.OriginRef — project, release, state,
-- object_version). Rows that predate this migration are backfilled from
-- their asset's origin project (the only origin the pre-T0701 model
-- recorded; origin_project_id is NOT NULL (00010), so every version row
-- gets a ref).
--
-- What the storage layer guarantees is narrower than "an origin": NOT
-- NULL (no absent column value), non-empty (at least one element), and
-- no NULL element (every element is a string). That is exactly the
-- schema's own type — array of string, minItems 1 — and no more: the
-- elements are text, so the database stores any string, including one
-- whose kind is unknown or whose value is not a uuid. The kind:value
-- shape is internal/assets.OriginRef's business (NewOriginRef, Valid),
-- enforced on the application path; duplicating that shape as a CHECK
-- here would mean two definitions of the origin vocabulary drifting
-- apart, in a column the schema does not constrain that way.

ALTER TABLE research_assets
  ADD COLUMN pid text NOT NULL
    DEFAULT substr(replace(gen_random_uuid()::text, '-', ''), 1, 26);

ALTER TABLE research_assets
  ADD CONSTRAINT research_assets_pid_format
    CHECK (pid ~ '^[0-9a-hjkmnp-tv-z]{26}$');

CREATE UNIQUE INDEX research_assets_pid_uniq ON research_assets (pid);

ALTER TABLE research_asset_versions
  ADD COLUMN origin_refs text[];

-- The backfill writes one field of existing version rows, which 00014's
-- row trigger forbids — for application writes, and rightly: a version
-- row is immutable history. A schema migration is the schema's own
-- authority, and a backfill of a column the old schema did not have is
-- data repair, not history rewriting (nothing previously stored is
-- changed; an absent field is filled in once). The guard is lifted for
-- exactly this statement and restored before the migration ends; goose
-- applies the whole migration in one transaction, so any failure rolls
-- the disable back with the rest of it, and after this migration no
-- path — application code, psql, a future migration — updates version
-- rows again. The statement-level TRUNCATE guard (00015) is unaffected.
ALTER TABLE research_asset_versions DISABLE TRIGGER research_asset_versions_append_only;

UPDATE research_asset_versions rav
  SET origin_refs = ARRAY['project:' || ra.origin_project_id::text]
  FROM research_assets ra
  WHERE ra.id = rav.asset_id
    AND rav.origin_refs IS NULL;

ALTER TABLE research_asset_versions ENABLE TRIGGER research_asset_versions_append_only;

ALTER TABLE research_asset_versions
  ALTER COLUMN origin_refs SET NOT NULL;

ALTER TABLE research_asset_versions
  ADD CONSTRAINT research_asset_versions_origin_refs_nonempty
    CHECK (cardinality(origin_refs) >= 1);

-- Non-empty is not yet "an origin": ARRAY[NULL] has cardinality 1 while
-- naming nothing. array_position compares with IS NOT DISTINCT FROM, so
-- it finds a NULL element (ARRAY[NULL] -> 1, ARRAY['a',NULL] -> 2) and
-- returns NULL only when no element is NULL — which is the passing
-- case. With this, a stored version row always carries at least one
-- actual string.
ALTER TABLE research_asset_versions
  ADD CONSTRAINT research_asset_versions_origin_refs_no_null
    CHECK (array_position(origin_refs, NULL) IS NULL);
