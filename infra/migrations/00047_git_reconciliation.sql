-- +goose Up
-- Git ↔ RSG reconciliation (T0309): the periodic drift check's canonical
-- record — one run row per verification pass, one append-only finding row
-- per drift instance, and one audit_log row per NEW finding (docs/16 §5:
-- every accepted state commit saves git commit/ref + rsg state hash; the
-- background reconciliation job checks drift; ANY drift is a high-severity
-- operational alert).
--
-- The reconciler (internal/gitprovider) verifies three dimensions per
-- pass:
--
--   branch ref  — a synced mapping row's provider ref must exist and its
--                 head must equal git_branch_refs.head_sha; a closed
--                 row's ref must be gone; a provider ref no branch row
--                 names must not exist (unmapped).
--   state hash  — every project_states row with a git_commit_sha must
--                 carry state_hash = GitStateHash(git_commit_sha) (the
--                 T0305-derived identity: the state's hash is a pure
--                 function of its pinned commit).
--   mapping     — the trigger-maintained invariants re-verified: 1:1
--                 branch ↔ git_branch_refs, the derived git_ref, the
--                 provision row of every provisioned project, and the
--                 provider repository behind every provision row.
--
-- The reconciler NEVER repairs: each finding carries a repair_proposal
-- (jsonb, the exact action a repair would take) and applies nothing —
-- the acceptance criterion "repair proposal 不静默修". A finding is
-- deduplicated while open (one open finding per kind+project+subject, the
-- partial unique index below), resolved by the pass that observes the
-- drift gone, and a RECURRING drift after resolution is a NEW finding (a
-- resolved row never reopens — the guard below). New findings append an
-- audit row (via='system', action='git.reconciliation.drift_detected',
-- severity 'high' in metadata) in the same transaction, so drift is
-- visible on the project's Activity feed without a new read surface.

CREATE TABLE git_reconciliation_runs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  -- A pass opens its run row up front (started_at) and fills the counts
  -- at the end (finished_at). A crashed or failed pass leaves the row
  -- unfinished — visible evidence that a pass did not complete, never a
  -- silently absent check.
  started_at timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz,
  refs_checked integer NOT NULL DEFAULT 0,
  states_checked integer NOT NULL DEFAULT 0,
  -- The broken mapping invariants the pass FOUND (checkMappings): one per
  -- violation row the canonical-side check returned — problems found, not
  -- points checked.
  mapping_violations integer NOT NULL DEFAULT 0,
  -- The provisioned repositories whose existence the pass VERIFIED
  -- (checkProvider): one per present repository — points checked, not
  -- problems found. A missing repository is a repository_missing finding,
  -- it is not counted here.
  repositories_checked integer NOT NULL DEFAULT 0,
  findings_opened integer NOT NULL DEFAULT 0,
  findings_open integer NOT NULL DEFAULT 0,
  findings_resolved integer NOT NULL DEFAULT 0,
  -- The provider failure that aborted the provider-side checks of this
  -- pass, when one did (redacted by construction — the adapter errors
  -- never carry the token or the configured URLs). Canonical-store checks
  -- still run and their findings still land; provider-side findings are
  -- neither created (a down provider would look like every ref missing —
  -- false findings) nor resolved (they were not re-verified).
  provider_error text
);

COMMENT ON TABLE git_reconciliation_runs IS
  'One row per Git ↔ RSG reconciliation pass (T0309): the pass''s scope counts and its outcome. The row is the reconciler''s own bookkeeping — opened at pass start, updated (never deleted) at pass end; a pass that crashed or failed stays unfinished, so a missing check is visible.';

CREATE TABLE git_reconciliation_findings (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  run_id uuid NOT NULL REFERENCES git_reconciliation_runs(id) ON DELETE RESTRICT,
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  kind text NOT NULL CHECK (kind IN ('ref_missing','ref_head_moved','dangling_ref','unmapped_ref','repository_missing','state_hash_mismatch','mapping_violation')),
  -- Every drift is high-severity by product rule (docs/16 §5) — the
  -- check pins the rule at the storage layer, the same discipline as
  -- 00031's guards.
  severity text NOT NULL DEFAULT 'high' CHECK (severity = 'high'),
  -- The stable subject of the finding: the git ref for ref findings, the
  -- state id for state-hash findings, the branch id for branch-mapping
  -- violations, "<owner>/<name>" for repository findings. The dedupe key
  -- below keeps at most one OPEN finding per (kind, project, subject).
  subject_ref text NOT NULL,
  -- The observed facts (expected vs actual) — per kind, stable field
  -- names so operators can read the finding without code.
  detail jsonb NOT NULL,
  -- What a repair WOULD do — an exact action (enqueue the branch-ref
  -- sync, record the unrecorded push, restore the derived state hash,
  -- re-provision), never applied by the reconciler. For findings whose
  -- repair is a semantic decision (an unmapped ref: adopt into the RSG
  -- or delete), the proposal names the options instead of choosing.
  repair_proposal jsonb NOT NULL,
  status text NOT NULL DEFAULT 'open' CHECK (status IN ('open','resolved')),
  created_at timestamptz NOT NULL DEFAULT now(),
  resolved_at timestamptz
);

COMMENT ON TABLE git_reconciliation_findings IS
  'One row per Git ↔ RSG drift instance (T0309): what drifted, the observed facts, the repair proposal that is NOT applied, and the finding''s lifecycle. Open findings dedupe per (kind, project, subject); a resolved finding stays resolved (the guard) and a recurring drift is a new row. Append-only except the status transition, and never deleted.';

-- One open finding per (kind, project, subject): the pass that observes a
-- drift again re-uses the open finding (no alert spam), and the pass that
-- observes it gone resolves it. The index doubles as the dedupe lookup.
CREATE UNIQUE INDEX git_reconciliation_findings_open_dedupe_idx
  ON git_reconciliation_findings (kind, project_id, subject_ref)
  WHERE status = 'open';

-- The finding lifecycle guard: rows are facts of their run — only the
-- status may transition, forward only (open → resolved), and a resolved
-- finding never reopens (a recurring drift is a new row, so each drift
-- episode keeps its own resolved_at). The resolved_at bookkeeping is
-- maintained here for ANY update path, like 00031's guards.
-- +goose StatementBegin
CREATE FUNCTION git_reconciliation_finding_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'git_reconciliation_findings rows are never deleted: a drift finding is a fact of its run'
      USING ERRCODE = 'P0001';
  END IF;
  IF NEW.id IS DISTINCT FROM OLD.id
     OR NEW.run_id IS DISTINCT FROM OLD.run_id
     OR NEW.project_id IS DISTINCT FROM OLD.project_id
     OR NEW.kind IS DISTINCT FROM OLD.kind
     OR NEW.severity IS DISTINCT FROM OLD.severity
     OR NEW.subject_ref IS DISTINCT FROM OLD.subject_ref
     OR NEW.detail IS DISTINCT FROM OLD.detail
     OR NEW.repair_proposal IS DISTINCT FROM OLD.repair_proposal THEN
    RAISE EXCEPTION 'git_reconciliation_findings content is immutable: only the status transitions'
      USING ERRCODE = 'P0001';
  END IF;
  IF OLD.status = 'resolved' AND NEW.status <> 'resolved' THEN
    RAISE EXCEPTION 'a resolved finding never reopens: a recurring drift is a new finding'
      USING ERRCODE = 'P0001';
  END IF;
  IF NEW.status = 'open' AND NEW.resolved_at IS NOT NULL THEN
    RAISE EXCEPTION 'an open finding has no resolved_at'
      USING ERRCODE = 'P0001';
  END IF;
  IF NEW.status = 'resolved' AND NEW.resolved_at IS NULL THEN
    NEW.resolved_at = now();
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER git_reconciliation_finding_guard_trigger
  BEFORE UPDATE OR DELETE ON git_reconciliation_findings
  FOR EACH ROW EXECUTE FUNCTION git_reconciliation_finding_guard();

-- The TRUNCATE halves (00015's discipline: row triggers do not fire on
-- TRUNCATE) — the drift history is history, not even wholesale erasure is
-- a legal update path. Runs are the reconciler's bookkeeping and are
-- updatable by design (the pass fills its counts), but a pass history is
-- still not truncated away.
CREATE TRIGGER git_reconciliation_findings_no_truncate
  BEFORE TRUNCATE ON git_reconciliation_findings FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER git_reconciliation_runs_no_truncate
  BEFORE TRUNCATE ON git_reconciliation_runs FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
