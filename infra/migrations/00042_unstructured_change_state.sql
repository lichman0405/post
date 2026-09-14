-- +goose Up
-- Unstructured change state (T0306): the branch semantic completeness flag
-- and the two DB-enforced gates that make an incomplete branch unmergeable.
--
-- docs/16 §4: after a push is ingested, the branch is marked
-- `semantic_complete` or `unstructured_changes`. Files the platform cannot
-- parse are RETAINED — every changed path of every push is recorded in
-- git_push_changes (00034), nothing is dropped — but a branch carrying
-- unresolved unstructured content must not reach a mergeable formal PR
-- until the Agent/API has filled in the required scientific semantics:
--   * pull_requests INSERT is refused while the source branch is
--     `unstructured_changes` (the branch cannot OPEN a formal PR);
--   * the branch lifecycle transition active -> merged is refused while the
--     flag is `unstructured_changes` (the merge cannot COMPLETE — the
--     backstop for any path that skips PR creation, today's branches.Merge
--     and T0409's merge governance alike).
-- Both gates are database triggers: they hold for ANY write path —
-- application code, psql, a leaked credential — the same discipline as
-- 00028/00031. That is what "不可绕过 semantic validation merge" means
-- mechanically: no code path can merge an incomplete branch.
--
-- The flag itself is a STORED PROJECTION, not a new fact: its only writer
-- is the push-ingestion store (internal/gitprovider), which recomputes it
-- from the append-only ingestion evidence inside the same transaction that
-- records the delivery and moves the head pointer — so flag and head can
-- never diverge observably. The derivation walks the branch's ingested
-- push chain from git_branch_refs.head_sha backwards over the
-- (before_sha, after_sha) links of git_push_ingestions and keeps, per
-- path, the nearest-to-head record in git_push_changes:
--   * unstructured added/modified  -> outstanding (the file exists at the
--     head in a form the platform does not understand);
--   * semantic_manifest            -> resolved (a later push replaced the
--     content with a known scientific manifest at the same path);
--   * removed                      -> resolved (the file is gone, there is
--     nothing left to understand).
-- A chain that ends at a commit without an ingestion row (a syncer-set
-- head, or the fork point) contributes no evidence and stops the walk —
-- the flag reports only what the evidence shows. The branch returns to
-- `semantic_complete` exactly when the evidence at the head resolves every
-- outstanding path. API state commits do NOT move the flag: there is no
-- linkage between an API commit and the files it would resolve, and
-- clearing on any commit would be the bypass the acceptance criterion
-- forbids.
--
-- One row per branch, born with the branch (branch_semantic_state_map,
-- the same pattern as 00031's branch_git_ref_map) and backfilled here for
-- branches that predate this migration. The row is upserted by ingestion;
-- it is deliberately NOT append-only (it is a projection of other tables'
-- append-only facts) and carries no transition guard — the CHECK on the
-- values is its shape constraint, and its sole writer is ingestion code.
--
-- Trust boundary (recorded, L1): the gates read the projection. A writer
-- that could UPDATE git_branch_semantic_states out-of-band is already a
-- direct-database actor of the kind every guard in this file set exists to
-- bound; the derivation's integrity is enforced by the ingestion store's
-- tests (the flag always equals the derivation from the change rows).

CREATE TABLE git_branch_semantic_states (
  branch_id uuid PRIMARY KEY REFERENCES branches(id) ON DELETE RESTRICT,
  semantic_state text NOT NULL DEFAULT 'semantic_complete'
    CHECK (semantic_state IN ('semantic_complete','unstructured_changes')),
  updated_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE git_branch_semantic_states IS
  'The branch semantic completeness flag (T0306, docs/16 §4): semantic_complete while the pushed content at the git head pointer is fully understood, unstructured_changes while at least one pushed path is an unresolved unstructured change. A projection of the append-only git_push_changes evidence, recomputed by the push-ingestion store in the delivery transaction; the pull_requests INSERT gate and the branch merge gate read it. Never appended: upserted per branch.';

-- Every branch that already exists gets its row (default: complete —
-- nothing was ever pushed and inspected for it); branches created from
-- here on get theirs from branch_semantic_state_map.
INSERT INTO git_branch_semantic_states (branch_id)
SELECT id FROM branches;

-- branch_semantic_state_map: one flag row per branch row, so a branch
-- without a flag row is impossible whatever path inserted it — the same
-- self-maintaining pattern as 00031's branch_git_ref_map.
-- +goose StatementBegin
CREATE FUNCTION branch_semantic_state_map() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  INSERT INTO git_branch_semantic_states (branch_id)
  VALUES (NEW.id);
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER branch_semantic_state_map_trigger
  AFTER INSERT ON branches
  FOR EACH ROW EXECUTE FUNCTION branch_semantic_state_map();

-- branch_merge_semantic_gate: the merge cannot COMPLETE while the branch
-- is unstructured_changes (docs/16 §4: the branch cannot become mergeable
-- until the required scientific semantics are filled). Fired on the
-- lifecycle transition to merged — the transition that lands the diff as
-- accepted state — and only on that transition: reaching merged is the
-- merge, staying merged is not a re-merge. A missing flag row (a branch
-- predating 00042 that was never backfilled — impossible, the backfill
-- above ran — or a manual row deletion) counts as complete: absence of
-- evidence is not evidence of unstructured content.
-- +goose StatementBegin
CREATE FUNCTION branch_merge_semantic_gate() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'UPDATE'
     AND NEW.lifecycle_state = 'merged'
     AND OLD.lifecycle_state IS DISTINCT FROM 'merged' THEN
    IF EXISTS (
      SELECT 1 FROM git_branch_semantic_states
      WHERE branch_id = NEW.id AND semantic_state = 'unstructured_changes'
    ) THEN
      RAISE EXCEPTION 'branch % cannot merge: its semantic state is unstructured_changes — the pushed content carries changes the platform cannot parse, and docs/16 §4 forbids merging until the required scientific semantics are filled',
        NEW.id
        USING ERRCODE = 'P0001';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER branch_merge_semantic_gate_trigger
  BEFORE UPDATE ON branches
  FOR EACH ROW EXECUTE FUNCTION branch_merge_semantic_gate();

-- pull_request_semantic_gate: the branch cannot OPEN a formal PR while it
-- is unstructured_changes. The gate checks the SOURCE branch at INSERT —
-- the branch whose diff the PR proposes. The PR row is created by T0402's
-- domain (not yet built); the gate exists now so the constraint is
-- canonical-store-level before any writer exists, exactly like 00009's
-- schema itself. An UPDATE that later refreshes the PR's proposed state
-- stays inside the same source branch; completeness at merge time is
-- re-checked by branch_merge_semantic_gate.
-- +goose StatementBegin
CREATE FUNCTION pull_request_semantic_gate() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM git_branch_semantic_states
    WHERE branch_id = NEW.source_branch_id AND semantic_state = 'unstructured_changes'
  ) THEN
    RAISE EXCEPTION 'pull request cannot be opened from branch %: its semantic state is unstructured_changes — the pushed content carries changes the platform cannot parse, and docs/16 §4 forbids a formal PR until the required scientific semantics are filled',
      NEW.source_branch_id
      USING ERRCODE = 'P0001';
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER pull_request_semantic_gate_trigger
  BEFORE INSERT ON pull_requests
  FOR EACH ROW EXECUTE FUNCTION pull_request_semantic_gate();

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
