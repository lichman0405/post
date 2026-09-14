-- +goose Up
-- Push ingestion (T0305): the canonical record of every verified push
-- webhook delivery, the inspected changed-file set, and the candidate
-- semantic diff derived from known scientific manifests (docs/16 §4:
-- webhook → inspect changed paths/manifests → validate known scientific
-- manifests → generate candidate RSG diff).
--
-- Idempotency is the acceptance criterion "重复 webhook 不重复 state":
-- the dedupe key is (gitea_repo_id, git_ref, after_sha) — the provider's
-- own per-attempt delivery id (X-Gitea-Delivery) is regenerated on every
-- redelivery (checked against the running instance), so it cannot dedupe;
-- the pushed head commit is the stable identity of the push's content.
-- Two deliveries of the same push therefore collide on the key and the
-- whole ingestion (changes, candidates, head pointer, state row) is a
-- no-op — INSERT ... ON CONFLICT DO NOTHING inside one transaction.
--
-- Consequence, by design: a force-push that returns a ref to a commit
-- whose tree was already ingested is skipped too — the state at that head
-- already exists, so nothing new to record.
--
-- The ref's head pointer is GUARDED, not a free fact of the push, in
-- both advance paths:
--   * a push whose before names the current head advances only while the
--     pointer is still there — head_sha = before_sha — so a delivery
--     whose before no longer matches is refused and the row marks
--     head_skip_reason = 'stale_before';
--   * a creation push (before is zeros) advances only while the ref is
--     still unborn — head_sha IS NULL — or already at the pushed head; a
--     creation delivery that arrives after the ref gained a head (a
--     later sync, or a newer push) is refused and the row marks
--     head_skip_reason = 'stale_creation'.
-- A refused advance still records the delivery in full (the audit rows
-- are facts of the delivery); only the pointer is held. An older push
-- whose first attempt 503'd and whose redelivery arrives after a newer
-- push was ingested therefore can never rewind the pointer past the
-- newer head, in either path. An exact duplicate is a complete no-op
-- including the pointer. Residual provider/state drift that no push can
-- express is T0309's reconciliation job, not this table's.
--
-- project_states: every pushed branch head is also recorded as a state
-- row (append-only, 00014-guarded — the provider commit arrives WITH the
-- row, never by UPDATE), so a later branch creation can fork its provider
-- ref at the base state's git_commit_sha (T0303's fork-point resolution)
-- and the branch's state history includes its git pushes. The state hash
-- is content-derived: sha256 over the canonical JSON {"git_commit_sha":
-- "<after>"} — the git-compat state identity, distinct from the domain's
-- transition hashes by construction (it hashes the pinned commit, not a
-- parent + operations pair).

CREATE TABLE git_push_ingestions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  -- The provider's per-attempt delivery id (X-Gitea-Delivery), for
  -- correlating with the provider's delivery log only — NOT the dedupe
  -- key (redeliveries mint a new one).
  delivery_id text,
  gitea_repo_id bigint NOT NULL,
  -- The canonical project, resolved from git_repository_provisions at
  -- ingestion time. NULL when the repository is not provisioned (a
  -- delivery for a repository this platform does not own — recorded for
  -- audit, mapped to no branch and no state).
  project_id uuid REFERENCES projects(id) ON DELETE RESTRICT,
  branch_name text NOT NULL,
  git_ref text NOT NULL,
  before_sha text NOT NULL,
  after_sha text NOT NULL,
  -- The provider's own commit count (may exceed the delivered commit
  -- list — the provider truncates it; the authoritative changed-path set
  -- comes from the git diff, never from the list).
  commit_count integer NOT NULL DEFAULT 0,
  -- Provider login of the pusher (not mapped to a platform user — T0304
  -- owns identity mapping).
  pusher text,
  -- The delivered commit list as the provider sent it (already bounded
  -- by the provider), for audit; the ingestion pipeline does not consume
  -- it.
  commits jsonb NOT NULL DEFAULT '[]'::jsonb,
  -- Why the head-pointer advance was refused for this delivery, when it
  -- was: 'stale_before' means git_branch_refs.head_sha no longer equalled
  -- before_sha when the delivery arrived — a stale redelivery of an older
  -- push (its first attempt failed before recording anything, and a newer
  -- push was ingested in between), or a ref that was never synced to the
  -- claimed base. 'stale_creation' means the delivery created the ref
  -- (before is zeros) but the ref already carried a head this push did
  -- not create — a creation push whose first attempt 503'd and whose
  -- redelivery arrived after a later sync or a newer push set the
  -- pointer. The refusal is recorded, never silent: the audit rows of
  -- the delivery are kept, only the pointer is not moved. NULL when the
  -- advance happened (or there was no pointer to guard).
  head_skip_reason text CHECK (head_skip_reason IS NULL OR head_skip_reason IN ('stale_before', 'stale_creation')),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (gitea_repo_id, git_ref, after_sha)
);

COMMENT ON TABLE git_push_ingestions IS
  'The canonical record of every accepted push webhook delivery (T0305): one row per distinct pushed head per ref per repository. The dedupe key (gitea_repo_id, git_ref, after_sha) makes redelivered webhooks a no-op — the acceptance criterion 重复 webhook 不重复 state. The head pointer advance is guarded: it happens only when the pointer is still where the push started, and a refused advance is recorded in head_skip_reason, never silently dropped. Rows are append-only (guard below).';

-- git_push_changes: the inspection result — every changed path of the
-- push, classified. semantic_manifest means the file''s content validated
-- against a known scientific schema; everything else is unstructured
-- (T0306 builds the branch completeness flag on this).
CREATE TABLE git_push_changes (
  ingestion_id uuid NOT NULL REFERENCES git_push_ingestions(id) ON DELETE RESTRICT,
  path text NOT NULL,
  change_kind text NOT NULL CHECK (change_kind IN ('added','modified','removed')),
  file_kind text NOT NULL CHECK (file_kind IN ('semantic_manifest','unstructured')),
  -- The matched schema id for semantic_manifest files (the registry''s
  -- canonical $id, e.g. https://open-rd.example/schemas/material.schema.json —
  -- the shape the validation surface stores); NULL for unstructured.
  schema_id text,
  -- sha256 hex of the file content the classification inspected (the
  -- pushed content for added/modified, the pre-push content for removed);
  -- NULL for unstructured files (never read) and oversize files.
  content_sha256 text,
  PRIMARY KEY (ingestion_id, path)
);

COMMENT ON TABLE git_push_changes IS
  'The inspected changed-file set of one ingested push (T0305): path, change kind and the manifest classification (semantic_manifest vs unstructured). The authoritative path set comes from the git diff — the provider payload''s commit list is truncated (checked against the running instance).';

-- git_push_semantic_candidates: the candidate RSG diff — one row per
-- changed manifest file whose content matched a known scientific schema.
-- The candidate is the PROPOSAL the push implies (create/update/delete of
-- the manifest''s content), not an applied semantic change: applying it
-- requires the semantic validation pipeline (T0207 machinery); until then
-- it stays status = ''candidate''. T0306 derives the branch completeness
-- flag from candidates vs unstructured changes.
CREATE TABLE git_push_semantic_candidates (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  ingestion_id uuid NOT NULL REFERENCES git_push_ingestions(id) ON DELETE RESTRICT,
  path text NOT NULL,
  change_kind text NOT NULL CHECK (change_kind IN ('added','modified','removed')),
  schema_id text NOT NULL,
  -- The manifest content the candidate proposes, bounded (the
  -- ingestion reads at most 1 MiB per file).
  candidate jsonb NOT NULL,
  -- candidate is the only state T0305 writes; later tasks move rows to
  -- validated (semantic validation passed), applied (the RSG transition
  -- committed) or rejected (validation failed).
  status text NOT NULL DEFAULT 'candidate' CHECK (status IN ('candidate','validated','applied','rejected')),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (ingestion_id, path)
);

COMMENT ON TABLE git_push_semantic_candidates IS
  'The candidate semantic diff of one ingested push (T0305): per changed manifest file, the proposed content and its matched schema, awaiting the semantic validation pipeline. Never an applied RSG change by itself.';

-- The ingestion record and the inspection result are append-only facts
-- (docs/21 §4, 00014's discipline): a delivery once recorded is never
-- rewritten — the dedupe no-op and the T0309 reconciliation both depend
-- on it.
CREATE TRIGGER git_push_ingestions_append_only
  BEFORE UPDATE OR DELETE ON git_push_ingestions
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER git_push_changes_append_only
  BEFORE UPDATE OR DELETE ON git_push_changes
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

-- The TRUNCATE halves (00015's discipline: row triggers do not fire on
-- TRUNCATE) — the push record is history, not even wholesale erasure is a
-- legal update path.
CREATE TRIGGER git_push_ingestions_no_truncate
  BEFORE TRUNCATE ON git_push_ingestions FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER git_push_changes_no_truncate
  BEFORE TRUNCATE ON git_push_changes FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER git_push_semantic_candidates_no_truncate
  BEFORE TRUNCATE ON git_push_semantic_candidates FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

-- Candidate rows are content-immutable but their status transitions
-- (candidate → validated | applied | rejected, the validation pipeline's
-- later tasks). The guard pins everything except the status: the proposed
-- content of a delivery is a fact of that delivery, and a candidate row is
-- never deleted (its ingestion is append-only).
-- +goose StatementBegin
CREATE FUNCTION git_push_semantic_candidate_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'git_push_semantic_candidates rows are never deleted: a candidate is a fact of its delivery'
      USING ERRCODE = 'P0001';
  END IF;
  IF NEW.ingestion_id IS DISTINCT FROM OLD.ingestion_id
     OR NEW.path IS DISTINCT FROM OLD.path
     OR NEW.change_kind IS DISTINCT FROM OLD.change_kind
     OR NEW.schema_id IS DISTINCT FROM OLD.schema_id
     OR NEW.candidate IS DISTINCT FROM OLD.candidate THEN
    RAISE EXCEPTION 'git_push_semantic_candidates content is immutable: only status transitions'
      USING ERRCODE = 'P0001';
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER git_push_semantic_candidate_guard_trigger
  BEFORE UPDATE OR DELETE ON git_push_semantic_candidates
  FOR EACH ROW EXECUTE FUNCTION git_push_semantic_candidate_guard();

-- The head-pointer refresh (T0303 assigned it to T0305): 00031's
-- git_branch_ref_guard forbids ANY update that does not move sync_state —
-- the syncer was the only writer when it was written. Push ingestion must
-- keep head_sha fresh without being a state-machine transition, so the
-- guard is REPLACED with the same machine plus a fast path: an update
-- that changes NOTHING but the tip pointer (head_sha; the guard itself
-- maintains updated_at) is allowed in every state except 'closed'
-- (terminal — a deleted ref's row does not reopen, its final head is
-- already recorded). The fast path is a WHITELIST over every other
-- column — to_jsonb(NEW) and to_jsonb(OLD) compared with head_sha and
-- updated_at removed — so a column added to this table later is covered
-- automatically (a fixed enumeration would let the next new column slip
-- through, as it let branch_id and created_at). The transition rules are
-- otherwise copied verbatim from 00031.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION git_branch_ref_guard() RETURNS trigger
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
    IF to_jsonb(NEW) - 'head_sha' - 'updated_at' = to_jsonb(OLD) - 'head_sha' - 'updated_at' THEN
      -- T0305 head-pointer refresh: the tip moved, nothing else did.
      NEW.updated_at = now();
      RETURN NEW;
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

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
