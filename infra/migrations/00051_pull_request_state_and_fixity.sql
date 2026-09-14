-- +goose Up
-- Pull request state machine and fixity invariants (task T0402).
--
-- The pull_requests row is a proposal whose address is its identity:
-- docs/09 §4 ("PR 包含：source/target branch、base state、RSG diff、…")
-- and docs/43 ("open → review_required → changes_requested/approved →
-- merge_ready → merged；也可 closed/aborted"). Three families of
-- invariants, enforced by the database itself for ANY update path —
-- application code, psql, a leaked credential — with the one stated
-- exception at the end of this header (the proposed-state gate):
--
--   1. state vocabulary: the CHECK names the canonical PR states; no
--      invalid state is storable;
--   2. lifecycle: pull_request_guard enforces the docs/43 transition
--      map — a terminal PR (merged/closed/aborted) never moves again,
--      the machine cannot be skipped (open → merged is refused at the
--      database), and merged_at is consistent (set on merged, cleared
--      only by leaving merged — which the map itself forbids);
--   3. fixity: the PR's base and proposed states are FIXED (task
--      requirement "base/proposed state fixed"). project, number and
--      the branch pair are structural identity and immutable too.
--      base_state_id never changes — the acceptance "PR base 不随 main
--      漂移" holds even against code that would re-read the target
--      branch's head. proposed_state_id moves ONLY through the explicit
--      head refresh: the transaction-scoped session flag
--      post.pr_head_refresh='on' (set by the refresh adapter path, and
--      only there) — the acceptance "head update 可显式 refresh", and
--      exactly one sanctioned write path for it.
--
-- Scope of the proposed-state gate, stated precisely: the flag blocks
-- IMPLICIT or accidental writes by application code — a rogue UPDATE
-- without the flag fails, and no query in this schema sets the flag.
-- It is NOT a defense against hostile or leaked SQL credentials: any
-- session with UPDATE privilege on pull_requests can run
-- set_config('post.pr_head_refresh','on',true) itself and move the
-- proposed state. Everything else in this migration (the CHECK, the
-- transition map, identity/base immutability, merged_at consistency)
-- is unconditional and holds even against such a session.
--
-- pull_requests is deliberately NOT covered by the append-only guards of
-- 00014: it is a mutable row by design (state, proposed head, merged_at).
-- This is the targeted constraint instead, the same discipline as 00028's
-- branch lifecycle guard. No existing constraint is changed or dropped;
-- no column semantics are added (rows exist only with state='open',
-- which the CHECK admits).

ALTER TABLE pull_requests ADD CONSTRAINT pull_requests_state_check
  CHECK (state IN ('open','review_required','changes_requested','approved',
                   'merge_ready','merged','closed','aborted'));

-- +goose StatementBegin
CREATE FUNCTION pull_request_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  valid_transition boolean;
BEGIN
  IF TG_OP = 'UPDATE' THEN
    -- Structural identity and the fixed base: immutable for the row's
    -- lifetime. The base state is pinned at creation and must not drift
    -- with the target branch's later heads.
    IF NEW.project_id IS DISTINCT FROM OLD.project_id
       OR NEW.number IS DISTINCT FROM OLD.number
       OR NEW.source_branch_id IS DISTINCT FROM OLD.source_branch_id
       OR NEW.target_branch_id IS DISTINCT FROM OLD.target_branch_id
       OR NEW.base_state_id IS DISTINCT FROM OLD.base_state_id THEN
      RAISE EXCEPTION 'pull request identity is fixed: project, number, branches and base state are immutable (docs/09 §4)'
        USING ERRCODE = 'P0001';
    END IF;
    -- The proposed head moves only through the explicit head refresh,
    -- and never on a terminal PR.
    IF NEW.proposed_state_id IS DISTINCT FROM OLD.proposed_state_id THEN
      IF OLD.state IN ('merged','closed','aborted') THEN
        RAISE EXCEPTION 'a terminal pull request never moves: the proposed state is fixed on a % PR (docs/43)',
          OLD.state
          USING ERRCODE = 'P0001';
      END IF;
      IF coalesce(current_setting('post.pr_head_refresh', true), '') <> 'on' THEN
        RAISE EXCEPTION 'pull request head moves only through the explicit head refresh (T0402)'
          USING ERRCODE = 'P0001';
      END IF;
    END IF;
    -- The docs/43 transition map: the machine cannot be skipped and a
    -- terminal state never moves again. The application's
    -- compare-and-swap is the normal path; this is the backstop.
    IF NEW.state IS DISTINCT FROM OLD.state THEN
      valid_transition :=
        (OLD.state = 'open' AND NEW.state IN ('review_required','closed','aborted'))
        OR (OLD.state = 'review_required' AND NEW.state IN ('changes_requested','approved','closed','aborted'))
        OR (OLD.state = 'changes_requested' AND NEW.state IN ('review_required','closed','aborted'))
        OR (OLD.state = 'approved' AND NEW.state IN ('merge_ready','closed','aborted'))
        OR (OLD.state = 'merge_ready' AND NEW.state IN ('merged','closed','aborted'));
      IF NOT valid_transition THEN
        RAISE EXCEPTION 'illegal pull request state transition: % -> % (docs/43)',
          OLD.state, NEW.state
          USING ERRCODE = 'P0001';
      END IF;
    END IF;
  END IF;
  -- merged_at consistency: only a merged PR carries a merge timestamp;
  -- reaching merged stamps it (any path gets consistent data).
  IF NEW.state = 'merged' AND NEW.merged_at IS NULL THEN
    NEW.merged_at = now();
  ELSIF NEW.state <> 'merged' AND NEW.merged_at IS NOT NULL THEN
    RAISE EXCEPTION 'merged_at is only set on a merged pull request (state %)', NEW.state
      USING ERRCODE = 'P0001';
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER pull_request_guard_trigger
  BEFORE INSERT OR UPDATE ON pull_requests
  FOR EACH ROW EXECUTE FUNCTION pull_request_guard();

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
