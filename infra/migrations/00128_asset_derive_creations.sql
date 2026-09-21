-- +goose Up
-- Asset fork/derive governance (T0708): the Idempotency-Key ledger for a
-- derivation — the write path that creates a NEW asset identity from an
-- existing published version and records the lineage edge
-- (docs/11 §5: "Fork/Derive：创建新的 Asset/Object identity，保留 lineage").
--
-- # Why this is not asset_publish_creations
--
-- The two ledgers have the same shape and they are deliberately not the
-- same table. A key's meaning is the command that consumed it: a derive
-- and a publish that carry one string are two different requests, and a
-- single key space would let one command answer for the other's row.
-- Concretely, the publish store's replay reads the ledger row and hands
-- back the version it names; if a derive had written that row with the
-- same key and version label, a publish repeating the key could be
-- answered with a version it never published — the caller would be told
-- its publication succeeded when all that happened was a derivation. Two
-- ledgers cannot be confused that way, and the cost of the second one is
-- one table.
--
-- # The extra two columns
--
-- The derivation's identity is not just the child version: it is the
-- (parent version, relation) pair the child was created FROM. Those two
-- columns are what make a replay comparable rather than merely findable —
-- a key replaying a derivation from a DIFFERENT parent, or with a
-- different relation, is answered IDEMPOTENCY_CONFLICT the way the
-- publish's key is, and the check needs the stored parent and relation to
-- compare against.
--
-- relation_type carries the same two values this task writes and no
-- third: the asset_lineage CHECK (00010) also admits 'supersedes', which
-- a derivation never records (docs/11 §7 gives supersession its own
-- lifecycle action). Spelling the narrower set here means a row no derive
-- could have written cannot be created by one either.
--
-- # Append-only
--
-- Both halves of the guard are taken from 00053/00063/00072: the 00014
-- row guard (BEFORE UPDATE OR DELETE) and the 00015 TRUNCATE guard
-- (BEFORE TRUNCATE, statement level), because a ledger that can be
-- truncated is not append-only and TRUNCATE fires no row trigger. A
-- replay is a read, never a rewrite.
--
-- UNIQUE(project_id, idempotency_key) is the correctness claim: two
-- concurrent derivations carrying one key either produce one ledger row
-- (and one child asset, one child version and one lineage edge) or fail
-- the second on this constraint. It is a database guarantee, not a
-- check-then-insert race — the command's own ledger read before the
-- transaction is an optimization, and the store re-reads under the
-- project row lock.

CREATE TABLE asset_derive_creations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  idempotency_key text NOT NULL,
  asset_version_id uuid NOT NULL REFERENCES research_asset_versions(id) ON DELETE RESTRICT,
  parent_asset_version_id uuid NOT NULL REFERENCES research_asset_versions(id) ON DELETE RESTRICT,
  relation_type text NOT NULL CHECK(relation_type IN ('forked_from','derived_from')),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(project_id, idempotency_key)
);

CREATE TRIGGER asset_derive_creations_append_only
  BEFORE UPDATE OR DELETE ON asset_derive_creations
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER asset_derive_creations_no_truncate
  BEFORE TRUNCATE ON asset_derive_creations FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

-- +goose Down
-- The table's two triggers go with it (DROP TABLE drops its own triggers),
-- so the rollback is total: no ledger, no triggers, no leftover function
-- reference. Dropping a ledger loses the idempotency record of every
-- derivation that ever ran, which is why this is a rollback of the
-- MIGRATION SET and not something a running deployment does — the same
-- reading 00110 states for its own Down section.
DROP TABLE asset_derive_creations;
