-- +goose Up
-- Conflict resolutions (T0407): the human decisions the Scientific
-- Conflict Resolution UI records for the three-way state triple
-- (base/source/target) one semantic merge is computed over (docs/09 §8:
-- Accept A / Accept B / Keep both versions / Create validation branch /
-- Scientific conflict 可在 main 中保持 contested/unresolved).
--
-- One row is one decision on one classified conflict of the detector
-- report (internal/rsg/conflict, T0405). The conflict identity is the
-- full classifier key — target kind + id, the conflict code, the fields
-- and payload keys the conflict covers and (identity conflicts) the
-- paired object — so a decision can never be mistaken for a decision on
-- a different conflict, even after the detector's classification
-- vocabulary grows. The triple's three states are pinned: a decision
-- stays bound to the exact states it was made about.
--
-- Decisions are overwritten in place (the latest decision wins, decided_by/
-- decided_at move); the overwrite itself is audit-logged by the store,
-- so no decision change disappears silently. There is no DELETE path —
-- an undecided conflict simply has no row; `unresolved` is an explicit
-- decision, not the absence of one.
CREATE TABLE conflict_resolutions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  base_state_id uuid NOT NULL REFERENCES project_states(id) ON DELETE RESTRICT,
  source_state_id uuid NOT NULL REFERENCES project_states(id) ON DELETE RESTRICT,
  target_state_id uuid NOT NULL REFERENCES project_states(id) ON DELETE RESTRICT,
  -- The conflict identity this decision addresses. target_id is the
  -- object id for object conflicts and the relation id for relation
  -- conflicts (the domain rows, not the version rows).
  target_kind text NOT NULL CHECK (target_kind IN ('object','relation')),
  target_id uuid NOT NULL,
  conflict_code text NOT NULL,
  conflict_fields jsonb NOT NULL DEFAULT '[]'::jsonb,
  conflict_payload_keys jsonb NOT NULL DEFAULT '[]'::jsonb,
  conflict_other_object_id uuid,
  -- The human decision. No computed outcome exists: the merge engine
  -- (T0406) executes exactly the kind chosen, and no kind averages or
  -- derives parameters (docs/09 §7: Protocol 冲突数值折中禁止自动).
  resolution text NOT NULL CHECK (resolution IN ('accept_source','accept_target','keep_both','validation_branch','unresolved')),
  -- The free-form reasoning the decider attaches (e.g. the name they
  -- propose for a validation branch).
  note text NOT NULL DEFAULT '',
  decided_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  decided_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

-- One decision per conflict identity per triple. NULLS NOT DISTINCT:
-- a conflict without a paired object (conflict_other_object_id NULL) is
-- still one identity — the default NULLS DISTINCT would let two rows for
-- the same NULL key accumulate and the overwrite would append instead of
-- replace.
CREATE UNIQUE INDEX conflict_resolutions_identity_idx ON conflict_resolutions
  (project_id, base_state_id, source_state_id, target_state_id,
   target_kind, target_id, conflict_code, conflict_fields,
   conflict_payload_keys, conflict_other_object_id) NULLS NOT DISTINCT;

-- The plan read: every decision of one triple, ordered for stable
-- rendering.
CREATE INDEX conflict_resolutions_plan_idx
  ON conflict_resolutions (project_id, base_state_id, source_state_id, target_state_id, target_kind, target_id);
