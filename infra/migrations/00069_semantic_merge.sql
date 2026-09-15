-- +goose Up
-- Semantic merge engine (task T0406): the record of one Research PR merge
-- and the conflicts that merge carries rather than resolves.
--
-- docs/09 §3 freezes main: the only way it advances is a Research PR
-- merge, and this migration is where that merge leaves its trace. A merge
-- row is written in the SAME transaction that commits the merged state
-- (semantic_merges.state_commit_id is NOT NULL by then), so the accepted
-- state and the record of how it was accepted cannot come apart. The row
-- also carries the plan that was executed — the engine's canonical JSON
-- (internal/rsg/merge) and its digest — so an auditor replays what the
-- merge decided without re-deriving it from the states.
--
-- Three things this migration is careful about:
--
--   1. one merge per pull request (UNIQUE(pull_request_id)): the PR state
--      machine reaches `merged` once (00051), and the database refuses a
--      second merge row for the same PR — trying to merge a PR twice
--      cannot double-advance the target branch;
--   2. the carried conflicts are APPEND-ONLY (the 00014 guard): a conflict
--      main accepted as contested/unresolved (docs/09 §8) is a fact of
--      that state. It is never edited away or deleted — the invariant
--      "nothing disappears; state only evolves" (CLAUDE.md §9) applies to
--      the open scientific disagreement exactly as it applies to the
--      content;
--   3. the Git saga columns are a separate, narrow update surface. The
--      GitProvider step happens AFTER the database transaction commits
--      (docs/08: Git is the repository/file truth beside PostgreSQL's
--      semantic truth), so the merge row is written first, the git outcome
--      is recorded second, and a failed git step stays visible and
--      retryable instead of silently diverging. The row itself is
--      otherwise immutable — the trigger below refuses any update outside
--      the saga columns, the same discipline as 00028's branch guard and
--      00051's PR guard.
--
-- The conflict_resolutions CHECK is widened to the FULL docs/09 §8
-- vocabulary. T0407 shipped the five kinds its page offered; T0406 must
-- express all seven actions, so the table has to store them. No existing
-- spelling changes and no row is rewritten — this is a strict widening.

-- 1. docs/09 §8: Accept A / Accept B / Keep both versions / Explicit
-- coexistence / Create validation branch / Request more evidence / Abort
-- proposed change, plus the contested/unresolved state main may keep.
ALTER TABLE conflict_resolutions DROP CONSTRAINT IF EXISTS conflict_resolutions_resolution_check;
ALTER TABLE conflict_resolutions ADD CONSTRAINT conflict_resolutions_resolution_check
  CHECK (resolution IN ('accept_source','accept_target','keep_both','explicit_coexistence',
                        'validation_branch','request_evidence','abort_change','unresolved'));

-- 2. One Research PR merge.
CREATE TABLE semantic_merges (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  -- The Research PR this merge executes. NOT NULL on purpose: main (and
  -- every other target branch) advances only this way (docs/09 §3/§4).
  pull_request_id uuid NOT NULL REFERENCES pull_requests(id) ON DELETE RESTRICT,
  source_branch_id uuid NOT NULL REFERENCES branches(id) ON DELETE RESTRICT,
  target_branch_id uuid NOT NULL REFERENCES branches(id) ON DELETE RESTRICT,
  -- The three states the plan was computed over, pinned: base (the PR's
  -- fixed base state), source (the proposed head at merge time), target
  -- (the target branch's head at merge time, under the row lock the merge
  -- holds). A merge is exactly this triple, so it can always be replayed.
  base_state_id uuid NOT NULL REFERENCES project_states(id) ON DELETE RESTRICT,
  source_state_id uuid NOT NULL REFERENCES project_states(id) ON DELETE RESTRICT,
  target_state_id uuid NOT NULL REFERENCES project_states(id) ON DELETE RESTRICT,
  -- The accepted state this merge committed. Written in the merge
  -- transaction (the commit's own write callback receives the state id):
  -- a merge row without a resulting state would be a record of nothing.
  -- The state_commit row that records the transition is the one whose
  -- (branch_id, result_state_id) match this row's target branch and result
  -- state — the commit row is inserted after its own write callback runs,
  -- so it cannot be referenced from inside the transaction that creates
  -- both.
  result_state_id uuid NOT NULL REFERENCES project_states(id) ON DELETE RESTRICT,
  actor_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  -- The executed plan: internal/rsg/merge's canonical JSON. plan_digest is
  -- sha256 over those exact bytes, so "the same three states and the same
  -- decisions produce the same merge" is checkable by recomputing it.
  plan_version text NOT NULL,
  plan jsonb NOT NULL,
  plan_digest text NOT NULL,
  -- What the plan did, rolled up (the PR's merge summary). Withheld counts
  -- the changes this merge did NOT publish — see withheld in the plan.
  applied_count int NOT NULL DEFAULT 0,
  kept_target_count int NOT NULL DEFAULT 0,
  carried_count int NOT NULL DEFAULT 0,
  aborted_count int NOT NULL DEFAULT 0,
  withheld_count int NOT NULL DEFAULT 0,
  -- The Git saga. `pending` is the state the merge transaction leaves: the
  -- database truth is committed, the GitProvider ref update has not
  -- happened yet. `updated` carries the merge commit sha; `failed` carries
  -- the error and stays retryable (attempts is the retry counter, so a
  -- stuck saga is visible instead of silent).
  git_ref text,
  git_sha text,
  git_state text NOT NULL DEFAULT 'pending'
    CHECK (git_state IN ('pending','updated','failed','skipped')),
  git_error text NOT NULL DEFAULT '',
  git_attempts int NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  -- A PR merges once (docs/43: `merged` is terminal); a second merge row
  -- for the same PR is a double-advance attempt and is refused here.
  UNIQUE (pull_request_id)
);

CREATE INDEX semantic_merges_project_created_idx
  ON semantic_merges (project_id, created_at DESC, id);

-- The saga's retry scan: merges whose Git step still has to happen.
CREATE INDEX semantic_merges_git_pending_idx
  ON semantic_merges (git_state, created_at)
  WHERE git_state <> 'updated';

-- 3. The conflicts this merge CARRIES instead of resolving (docs/09 §8:
-- keep both versions / explicit coexistence / contested-unresolved left in
-- main). One row per conflict of the detector's report; both side versions
-- are named, and so is the human whose decision kept them.
--
-- This table is append-only (see the trigger below): it is the record that
-- main contains an open scientific disagreement. A later decision is a new
-- merge, never an edit here.
CREATE TABLE semantic_merge_conflicts (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  merge_id uuid NOT NULL REFERENCES semantic_merges(id) ON DELETE RESTRICT,
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  result_state_id uuid NOT NULL REFERENCES project_states(id) ON DELETE RESTRICT,
  -- The conflict identity: the same classifier key conflict_resolutions
  -- uses, so the carried record and the human decision it came from are
  -- unmistakably the same conflict.
  target_kind text NOT NULL CHECK (target_kind IN ('object','relation')),
  target_id uuid NOT NULL,
  conflict_code text NOT NULL,
  -- The docs/09 §6 category the detector classified it under.
  conflict_category text NOT NULL,
  conflict_fields jsonb NOT NULL DEFAULT '[]'::jsonb,
  conflict_payload_keys jsonb NOT NULL DEFAULT '[]'::jsonb,
  other_object_id uuid,
  detail text NOT NULL DEFAULT '',
  -- The decision that carried it (the two "keep both" actions and the
  -- explicit unresolved state); Accept A/B and Abort never reach here.
  decision text NOT NULL CHECK (decision IN ('keep_both','explicit_coexistence','unresolved')),
  decided_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  note text NOT NULL DEFAULT '',
  -- Both versions stay — that is what carrying the conflict means.
  source_version_id uuid NOT NULL,
  target_version_id uuid,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX semantic_merge_conflicts_merge_idx
  ON semantic_merge_conflicts (merge_id, target_kind, target_id);

-- The read a later decision or the PR page makes: "is this target still
-- contested in the accepted state?"
CREATE INDEX semantic_merge_conflicts_target_idx
  ON semantic_merge_conflicts (project_id, target_kind, target_id, created_at DESC);

CREATE TRIGGER semantic_merge_conflicts_append_only
  BEFORE UPDATE OR DELETE ON semantic_merge_conflicts
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

-- Row-level triggers do not fire on TRUNCATE (the 00015 lesson): without
-- this, `TRUNCATE semantic_merge_conflicts CASCADE` would erase every
-- carried conflict in one statement.
CREATE TRIGGER semantic_merge_conflicts_no_truncate
  BEFORE TRUNCATE ON semantic_merge_conflicts FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

-- 4. semantic_merges is mutable in exactly one dimension: the Git saga. The
-- database truth (states, plan, counts, actor) is fixed once written —
-- anything else would let a merge record drift away from the state it
-- claims to have produced.
-- +goose StatementBegin
CREATE FUNCTION semantic_merge_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'UPDATE' THEN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.project_id IS DISTINCT FROM OLD.project_id
       OR NEW.pull_request_id IS DISTINCT FROM OLD.pull_request_id
       OR NEW.source_branch_id IS DISTINCT FROM OLD.source_branch_id
       OR NEW.target_branch_id IS DISTINCT FROM OLD.target_branch_id
       OR NEW.base_state_id IS DISTINCT FROM OLD.base_state_id
       OR NEW.source_state_id IS DISTINCT FROM OLD.source_state_id
       OR NEW.target_state_id IS DISTINCT FROM OLD.target_state_id
       OR NEW.result_state_id IS DISTINCT FROM OLD.result_state_id
       OR NEW.actor_id IS DISTINCT FROM OLD.actor_id
       OR NEW.plan_version IS DISTINCT FROM OLD.plan_version
       OR NEW.plan IS DISTINCT FROM OLD.plan
       OR NEW.plan_digest IS DISTINCT FROM OLD.plan_digest
       -- The counts are "what this merge did" (docs/09 §8's roll-up of the
       -- plan: applied / kept target / carried / aborted / withheld). They
       -- are part of the record exactly as much as the plan they summarise:
       -- a merge whose applied_count could be edited to disagree with the
       -- plan it stored would be a record that lies about the state it
       -- produced. Same for created_at, the merge's place in the history.
       OR NEW.applied_count IS DISTINCT FROM OLD.applied_count
       OR NEW.kept_target_count IS DISTINCT FROM OLD.kept_target_count
       OR NEW.carried_count IS DISTINCT FROM OLD.carried_count
       OR NEW.aborted_count IS DISTINCT FROM OLD.aborted_count
       OR NEW.withheld_count IS DISTINCT FROM OLD.withheld_count
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
      RAISE EXCEPTION 'semantic merge % is immutable: only the Git saga columns move (T0406)', OLD.id
        USING ERRCODE = 'P0001';
    END IF;
    -- A finished Git step is not rewritten: updated is terminal for the
    -- ref it names (a later merge is a new row).
    IF OLD.git_state = 'updated' AND NEW.git_state <> 'updated' THEN
      RAISE EXCEPTION 'semantic merge % already updated its Git refs (sha %); the saga is finished', OLD.id, OLD.git_sha
        USING ERRCODE = 'P0001';
    END IF;
    NEW.updated_at := now();
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER semantic_merge_guard_trigger
  BEFORE UPDATE ON semantic_merges
  FOR EACH ROW EXECUTE FUNCTION semantic_merge_guard();

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
