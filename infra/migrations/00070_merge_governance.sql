-- +goose Up
-- Merge governance (task T0409): the Idempotency-Key ledger for the
-- Research PR merge endpoint.
--
-- docs/22 lists merge among the commands an Idempotency-Key governs: a
-- client that retries `POST /projects/{projectId}/pull-requests/{number}:merge`
-- after a network failure must get the FIRST merge back, not a second
-- one. A second merge is not a second row here — semantic_merges is
-- UNIQUE(pull_request_id) (00069) and the PR state machine reaches
-- `merged` once (00051), so the write itself is already single-shot. What
-- the ledger adds is the ANSWER: without it a retry would be told
-- "PR_ALREADY_MERGED", which is a conflict, not the accepted result the
-- first attempt produced.
--
-- The ledger copies release_creations (00053) exactly, because the
-- contract it implements is the same one (docs/22 §3, idempotent
-- create/command/publish/merge/release):
--
--   * UNIQUE(project_id, idempotency_key) — one key names one merge per
--     project, and the uniqueness is a database guarantee rather than a
--     check-then-insert race between two concurrent retries;
--   * the mesh is read-then-insert INSIDE the merge's transaction, so a
--     concurrent duplicate loses on the unique index and the whole
--     transaction (state commit included) rolls back;
--   * append-only, both halves: the 00014 row guard (no UPDATE/DELETE)
--     and the 00015 TRUNCATE guard. A replay is a read, never a rewrite —
--     the ledger entry is part of the merge's history, so it obeys the
--     same "nothing disappears" rule as the merge row it points at.
--
-- The merge_id FK is ON DELETE RESTRICT rather than CASCADE for the same
-- reason release_creations uses RESTRICT: a ledger entry without its
-- merge would silently turn a replay into a miss (the key would look
-- unused and a retry would try to merge again).

CREATE TABLE merge_creations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  idempotency_key text NOT NULL,
  merge_id uuid NOT NULL REFERENCES semantic_merges(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(project_id, idempotency_key)
);

CREATE TRIGGER merge_creations_append_only
  BEFORE UPDATE OR DELETE ON merge_creations
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER merge_creations_no_truncate
  BEFORE TRUNCATE ON merge_creations FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();
