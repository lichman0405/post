-- +goose Up
-- Asset publish governance (T0705): the Idempotency-Key ledger for the
-- publication of a research asset version (docs/22: an Idempotency-Key
-- governs create/command/publish/merge/release; docs/23 §4 lists the
-- publish operation itself).
--
-- A publish is a governance write, not a create that can be repeated: it
-- widens a private asset into the network's view and its result — the
-- version row — is immutable (invariant 5, and the 00014 row trigger on
-- research_asset_versions). So a retried publish MUST NOT write a second
-- version, and it must not be answered with a conflict either: the
-- request that already succeeded is the request that is being repeated,
-- and the honest answer is the version it produced. This ledger is what
-- makes that answer possible after the fact — the key maps to the
-- version the FIRST request wrote, and a replay is a read of that row.
--
-- The three existing ledgers cannot carry it, and neither could a shared
-- one:
--
--   - release_creations (00053) has a NOT NULL release_id foreign key:
--     a publish rows nothing into releases.
--   - project_milestone_creations (00063) has a NOT NULL milestone_id:
--     a publish rows nothing into project_milestones.
--   - research_asset_versions itself is keyed by (asset_id, version), and
--     the key that has to replay is the CALLER's opaque string, not a
--     version label the caller chose. Two different keys naming the same
--     version are two different requests (the second is refused by the
--     immutability guard, which is a different outcome than a replay),
--     and one key is scoped to ONE project — the scope the other two
--     ledgers use, and the scope the key is looked up in.
--
-- Append-only, like the version row it points at and like the two other
-- ledgers: a replay is a read, never a rewrite. Both halves of the guard
-- are taken from 00053/00063 — the 00014 row guard (BEFORE UPDATE OR
-- DELETE) and the 00015 TRUNCATE guard (BEFORE TRUNCATE, statement
-- level), because a ledger that can be truncated is not append-only, and
-- TRUNCATE fires no row trigger.
--
-- UNIQUE(project_id, idempotency_key) is the whole correctness claim: the
-- commit of two concurrent publishes carrying one key either creates one
-- ledger row (and one version) or fails the second on this constraint —
-- which the store answers as a replay of the winner, never as a second
-- version. It is a database guarantee, not a check-then-insert race.

CREATE TABLE asset_publish_creations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  idempotency_key text NOT NULL,
  asset_version_id uuid NOT NULL REFERENCES research_asset_versions(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(project_id, idempotency_key)
);

CREATE TRIGGER asset_publish_creations_append_only
  BEFORE UPDATE OR DELETE ON asset_publish_creations
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER asset_publish_creations_no_truncate
  BEFORE TRUNCATE ON asset_publish_creations FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();
