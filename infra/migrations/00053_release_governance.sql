-- +goose Up
-- Immutable release governance (T0606): the release row is a complete,
-- self-contained snapshot — the full manifest document, never a join over
-- live state (an old release must render identically no matter how the
-- current state evolves, acceptance: 旧 release 不受 current state 改变).
--
-- The manifest column becomes text: the document is content-addressed —
-- manifest_hash is the sha256 of the canonical bytes, and the export is
-- served verbatim from the stored row. jsonb normalizes key order and
-- whitespace, so the stored bytes would no longer be the bytes the hash
-- was computed over and verification could never close (found by the
-- T0606 e2e: the manifest endpoint 503'd on VerifyHash after the jsonb
-- round trip reordered the nested documents). A byte-preserving column
-- type is the whole point of an immutable snapshot.
--
-- The release manifest pins TWO policy versions (docs/11 §1: the
-- organization lower bound and the project's stricter overlay, docs/12
-- §5); the canonical releases table carried one policy_version_id (the
-- project policy), so the organization pin gets its own column.
--
-- release_creations is the Idempotency-Key ledger (docs/22: an
-- Idempotency-Key governs create/command/publish/merge/release): a key
-- replays the release it created, forever. Append-only like the release
-- itself — a replay is a read, never a rewrite (migration 00014's guard
-- covers releases already; the ledger gets the same treatment, both
-- halves: the 00014 row guard and the 00015 TRUNCATE guard).
--
-- The list index orders a project's releases newest-first without a
-- sort; UNIQUE(project_id, version) keeps the same-version conflict a
-- database guarantee, never a check-then-insert race.

ALTER TABLE releases
  ALTER COLUMN manifest TYPE text USING manifest::text;

ALTER TABLE releases
  ADD COLUMN org_policy_version_id uuid REFERENCES policy_versions(id) ON DELETE RESTRICT;

CREATE TABLE release_creations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  idempotency_key text NOT NULL,
  release_id uuid NOT NULL REFERENCES releases(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(project_id, idempotency_key)
);

CREATE TRIGGER release_creations_append_only
  BEFORE UPDATE OR DELETE ON release_creations
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER release_creations_no_truncate
  BEFORE TRUNCATE ON release_creations FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

CREATE INDEX releases_project_created_idx
  ON releases(project_id, created_at DESC);
