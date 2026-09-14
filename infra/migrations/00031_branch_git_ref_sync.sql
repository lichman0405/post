-- +goose Up
-- Semantic branch ↔ Git ref consistency (T0303): the canonical mapping
-- between a branches row and its provider-side ref, and the invariants
-- that make "semantic branch 与 Git ref 一致" unconditional.
--
-- The ref name is DERIVED, never stored twice as an independent fact:
-- branches.git_ref = 'refs/heads/' || name is enforced by
-- branch_git_ref_guard for ANY write path (application code, psql, a leaked
-- credential) — the same discipline as 00028's lifecycle guard. The
-- application service derives the same value on creation
-- (branches.Service.Create), so existing writers are unaffected.
--
-- One mapping row per branches row, created by branch_git_ref_map —
-- branch creation and the mapping can never drift apart, no matter which
-- path inserts the branch.
--
-- Sync state machine (git_branch_refs.sync_state), driven by
-- internal/gitprovider's BranchRefSyncer (the boot sweep plus redelivered
-- jobs re-attempt, the same bounded-retry policy as T0301 provisioning):
--
--   pending → synced | failed | closing      row awaits its ref
--   synced → failed | closing                ref exists, head recorded
--   failed → synced | closing | closed | failed   last attempt failed; retryable
--   closing → closed | failed                branch closed; ref deletion pending
--   closed                                   terminal: ref deleted, final head recorded
--
-- The direction is carried by the row itself: close_requested_at set (or
-- sync_state = 'closing') means "delete the ref"; otherwise "ensure the ref
-- exists". git_branch_ref_guard enforces the machine for ANY update path.
--
-- Ref strategy on branch close (docs/43: active → merged | aborted): the
-- provider ref is DELETED in both cases, after its final head sha is
-- recorded in head_sha (the reconstruction pointer). The semantic history
-- is unaffected — branches / project_states / state_commits stay immutable
-- (00028) — and docs/46 retention applies to scientific history, not to
-- git ref pointers: a dangling research ref must not keep accepting pushes
-- once the semantic path is closed. The deletion is asynchronous:
-- branch_git_ref_close marks the row 'closing' and the syncer performs the
-- provider call; a provider outage leaves the row in the retry backlog.
--
-- main is mapped too (one row per branch, no exceptions). The syncer never
-- creates or deletes refs/heads/main on purpose: for main the fork point
-- resolves to the repository's default branch, which in an empty
-- repository does not exist — so main's row waits (retryable 'failed')
-- until T0302 creates the ref, then adopts it and records the head.
-- main's ref lifecycle belongs to T0302 (initial commit + protection) and
-- the T0406 merge service, not to this table's syncer.

CREATE TABLE git_branch_refs (
  branch_id uuid PRIMARY KEY REFERENCES branches(id) ON DELETE RESTRICT,
  git_ref text NOT NULL,
  -- fork_sha: the provider-side commit the ref was created at — the base
  -- state's project_states.git_commit_sha when recorded (T0305 fills those
  -- on ingestion), otherwise the repository's default branch was used (the
  -- pre-T0305 fallback; every state is an ancestor of the accepted head in
  -- the normal flow).
  fork_sha text,
  -- head_sha: the ref's last known tip, written by the syncer at create,
  -- adopt and close time. On close it is the final head recorded BEFORE
  -- the ref is deleted — the pointer that keeps the closed path
  -- reconstructable. T0305 keeps it fresh on push ingestion.
  head_sha text,
  sync_state text NOT NULL DEFAULT 'pending'
    CHECK (sync_state IN ('pending','synced','failed','closing','closed')),
  close_requested_at timestamptz,
  synced_at timestamptz,
  closed_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE git_branch_refs IS
  'The semantic branch → GitProvider ref mapping and sync record (T0303): one row per branches row (maintained by trigger), the provider-side fork/head commits, and the sync state machine that keeps semantic branch and Git ref consistent. Provider-side calls are internal/gitprovider''s alone; every other product path may only read head_sha.';

COMMENT ON COLUMN git_branch_refs.head_sha IS
  'Last known provider-side tip of the ref; on close, the final head recorded before deletion (the reconstruction pointer — docs/46 retention applies to semantic history; a closed git ref must not keep accepting pushes).';

-- ---------------------------------------------------------------------------
-- branch_git_ref_guard: the branches-side invariants.
--   1. git_ref is DERIVED — it must always equal 'refs/heads/' || name
--      (the 1:1 mapping the acceptance criterion names);
--   2. the branch name is immutable: the git ref is the branch's address,
--      so renaming would be a new branch (V1 has no rename path; a future
--      rename feature changes this migration deliberately, not by accident).
-- ---------------------------------------------------------------------------
-- +goose StatementBegin
CREATE FUNCTION branch_git_ref_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.git_ref <> 'refs/heads/' || NEW.name THEN
    RAISE EXCEPTION 'branch git_ref must be refs/heads/<name>: % does not name %',
      NEW.git_ref, NEW.name
      USING ERRCODE = 'P0001';
  END IF;
  IF TG_OP = 'UPDATE' AND NEW.name IS DISTINCT FROM OLD.name THEN
    RAISE EXCEPTION 'branch name is immutable: the git ref is the branch address, renaming is a new branch'
      USING ERRCODE = 'P0001';
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER branch_git_ref_guard_trigger
  BEFORE INSERT OR UPDATE ON branches
  FOR EACH ROW EXECUTE FUNCTION branch_git_ref_guard();

-- branch_git_ref_map: every branch row gets its mapping row, so the sync
-- backlog is self-maintaining — a branch that exists but is unmapped is
-- impossible, whatever path inserted it. A branch born closed (lifecycle
-- already merged/aborted at insert) starts in 'closing': its ref must be
-- removed, never created.
-- +goose StatementBegin
CREATE FUNCTION branch_git_ref_map() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.lifecycle_state IN ('merged','aborted') THEN
    INSERT INTO git_branch_refs (branch_id, git_ref, sync_state, close_requested_at)
    VALUES (NEW.id, NEW.git_ref, 'closing', now());
  ELSE
    INSERT INTO git_branch_refs (branch_id, git_ref)
    VALUES (NEW.id, NEW.git_ref);
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER branch_git_ref_map_trigger
  AFTER INSERT ON branches
  FOR EACH ROW EXECUTE FUNCTION branch_git_ref_map();

-- branch_git_ref_close: the abort/merge ref strategy's storage half — when
-- the semantic lifecycle closes (00028's guard enforces the active →
-- merged | aborted direction itself), the mapping row moves to 'closing':
-- the syncer then records the final head and deletes the provider ref.
-- The lifecycle transition commits first; the provider call happens later
-- in the job loop, so a provider outage can never roll back a semantic
-- merge.
-- +goose StatementBegin
CREATE FUNCTION branch_git_ref_close() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF OLD.lifecycle_state = 'active' AND NEW.lifecycle_state IN ('merged','aborted') THEN
    UPDATE git_branch_refs
       SET sync_state = 'closing', close_requested_at = now(), updated_at = now()
     WHERE branch_id = NEW.id;
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER branch_git_ref_close_trigger
  AFTER UPDATE OF lifecycle_state ON branches
  FOR EACH ROW EXECUTE FUNCTION branch_git_ref_close();

-- git_branch_ref_guard: the mapping-row invariants — the stored git_ref
-- matches the branch's derived ref (backstop for the guard above and for
-- the backfill below), the sync state machine only moves forward, and
-- 'closed' is terminal (a deleted ref's row never reopens).
-- +goose StatementBegin
CREATE FUNCTION git_branch_ref_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  want text;
BEGIN
  SELECT 'refs/heads/' || name INTO want FROM branches WHERE id = NEW.branch_id;
  IF want IS NULL OR NEW.git_ref <> want THEN
    RAISE EXCEPTION 'git_branch_refs.git_ref must name its branch: % does not name branch %',
      NEW.git_ref, NEW.branch_id
      USING ERRCODE = 'P0001';
  END IF;
  IF TG_OP = 'INSERT' AND NEW.sync_state = 'closed' THEN
    RAISE EXCEPTION 'a mapping row is born with work to do: closed is only reached through closing'
      USING ERRCODE = 'P0001';
  END IF;
  IF TG_OP = 'UPDATE' THEN
    IF NEW.git_ref IS DISTINCT FROM OLD.git_ref THEN
      RAISE EXCEPTION 'git_branch_refs.git_ref is immutable'
        USING ERRCODE = 'P0001';
    END IF;
    IF OLD.sync_state = 'closed' THEN
      RAISE EXCEPTION 'closed is terminal: a deleted ref''s row does not reopen'
        USING ERRCODE = 'P0001';
    END IF;
    IF OLD.sync_state = 'pending' AND NEW.sync_state NOT IN ('synced','failed','closing') THEN
      RAISE EXCEPTION 'pending may only move to synced, failed or closing'
        USING ERRCODE = 'P0001';
    END IF;
    IF OLD.sync_state = 'synced' AND NEW.sync_state NOT IN ('failed','closing') THEN
      RAISE EXCEPTION 'synced may only move to failed or closing'
        USING ERRCODE = 'P0001';
    END IF;
    IF OLD.sync_state = 'failed' AND NEW.sync_state NOT IN ('synced','closing','failed','closed') THEN
      RAISE EXCEPTION 'failed may only retry, reach synced, close, or reach closed'
        USING ERRCODE = 'P0001';
    END IF;
    IF OLD.sync_state = 'closing' AND NEW.sync_state NOT IN ('closed','failed') THEN
      RAISE EXCEPTION 'closing may only reach closed or fail back for retry'
        USING ERRCODE = 'P0001';
    END IF;
  END IF;
  NEW.updated_at = now();
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER git_branch_ref_guard_trigger
  BEFORE INSERT OR UPDATE ON git_branch_refs
  FOR EACH ROW EXECUTE FUNCTION git_branch_ref_guard();

-- Backfill: branches that predate this migration get their mapping row,
-- derived from the name (never copied from a possibly drifted git_ref).
-- Closed branches (merged/aborted) are born 'closing' with a close
-- timestamp, mirroring branch_git_ref_map above: their refs must be
-- removed by the sweep, never created — a dangling ref must not keep
-- accepting pushes once the semantic path is closed, and no close trigger
-- can ever fire for those terminal rows again (00028). Live branches start
-- 'pending': the next startup sweep adopts or creates every ref.
INSERT INTO git_branch_refs (branch_id, git_ref, sync_state, close_requested_at)
SELECT id, 'refs/heads/' || name,
       CASE WHEN lifecycle_state IN ('merged','aborted') THEN 'closing' ELSE 'pending' END,
       CASE WHEN lifecycle_state IN ('merged','aborted') THEN now() ELSE NULL END
FROM branches;

-- The sync backlog is the hot query (boot sweep + redelivered jobs); a
-- partial index keeps the terminal rows out of it. Same discipline as
-- 00026/00028 performance indexes.
CREATE INDEX git_branch_refs_sync_backlog_idx
  ON git_branch_refs (sync_state)
  WHERE sync_state IN ('pending','failed','closing');

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
