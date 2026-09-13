-- +goose Up
-- State snapshot projection indexes (task T0204). The two projection shapes
-- of the state model (docs/21 §5, docs/07 §7) are:
--
--   1. reading a state's direct members — the scientific object versions and
--      relation versions whose state_id equals the state (the transition each
--      row was created in); and
--   2. walking a branch's state lineage — the project_states chain and the
--      state_commits history, both filtered by branch_id.
--
-- All four projections are rebuildable from the canonical append-only
-- history; these indexes are performance-only, no semantic content.

CREATE INDEX scientific_object_versions_state_idx
  ON scientific_object_versions (state_id);

CREATE INDEX relation_versions_state_idx
  ON relation_versions (state_id);

CREATE INDEX project_states_branch_created_idx
  ON project_states (branch_id, created_at, id);

CREATE INDEX state_commits_branch_created_idx
  ON state_commits (branch_id, created_at, id);

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
