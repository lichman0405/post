-- +goose Up
-- Branch lifecycle invariants (task T0205): once a research branch is
-- merged or aborted its history is immutable (docs/43: "active → merged |
-- aborted；merged/aborted history immutable"). Enforced by the database
-- itself, for ANY update path — application code, psql, a leaked
-- credential:
--
--   1. the head pointer (branches.base_state_id, the docs/21 §5 "branch
--      current state" projection) must not move on a closed branch —
--      CommitState's compare-and-swap already guards it
--      (queries/rsg.sql UpdateBranchBaseState requires lifecycle_state =
--      'active'), this trigger makes the invariant unconditional;
--   2. the lifecycle itself is terminal: a merged/aborted branch never
--      transitions to another lifecycle (no merged→active, no merged→
--      aborted). The app-side CAS (SetBranchLifecycle requires the active
--      state) is the normal path; this trigger is the backstop.
--
-- branches is deliberately NOT covered by the append-only guards of
-- 00014: it is a mutable row by design (head projection, lifecycle).
-- This is the targeted constraint instead. No existing constraint is
-- changed or dropped; no column semantics are added.

-- +goose StatementBegin
CREATE FUNCTION branch_lifecycle_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF OLD.lifecycle_state <> 'active' AND NEW.lifecycle_state IS DISTINCT FROM OLD.lifecycle_state THEN
    RAISE EXCEPTION 'branch lifecycle is terminal: a % branch cannot move to % (docs/43: active -> merged | aborted only)',
      OLD.lifecycle_state, NEW.lifecycle_state
      USING ERRCODE = 'P0001';
  END IF;
  IF OLD.lifecycle_state <> 'active' AND NEW.base_state_id IS DISTINCT FROM OLD.base_state_id THEN
    RAISE EXCEPTION 'branch head is immutable on a % branch (merged/aborted history is immutable, docs/43)',
      OLD.lifecycle_state
      USING ERRCODE = 'P0001';
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER branch_lifecycle_guard_trigger
  BEFORE UPDATE ON branches
  FOR EACH ROW EXECUTE FUNCTION branch_lifecycle_guard();

-- Branches are listed per project in creation order
-- (ListBranchesByProject); performance-only index, same discipline as
-- 00026 — the UNIQUE(project_id, name) constraint does not cover the
-- ordering.
CREATE INDEX branches_project_created_idx
  ON branches (project_id, created_at, id);

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
