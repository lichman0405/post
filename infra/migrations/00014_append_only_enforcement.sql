-- +goose Up
-- Append-only enforcement (task T0013): the version/history/ledger tables are
-- immutable. An in-place UPDATE or DELETE of an existing row is rejected by
-- the database itself (BEFORE UPDATE OR DELETE row triggers that RAISE
-- EXCEPTION), so no client — application code, psql, a leaked credential — can
-- rewrite or erase history (CLAUDE.md §9, ADR-007, ADR-008, ADR-021,
-- docs/21 §4, docs/46, Master Acceptance Gate A).
--
-- Corrections never mutate a row: they append a new one (abort/reopen,
-- supersede, new version, new state). One shared guard function reports the
-- table name and the forbidden operation (TG_TABLE_NAME / TG_OP) so an
-- operator can act on the error.
--
-- Covered tables (append-only by design; see task result for the full
-- decision and the deliberately exempt tables):
--   scientific_object_versions, relation_versions        version logs
--   project_states, state_commits                        RSG state history
--   releases, research_asset_versions, asset_lineage     release/asset history
--   policy_versions                                      policy versions
--   validation_results                                   gate run records
--   contribution_events, audit_log                       append-only ledgers
--   research_events                                      domain event log
--   external_reference_snapshots                         pinned snapshots
--
-- No existing constraint is changed or dropped; no column semantics are added.

-- +goose StatementBegin
CREATE FUNCTION append_only_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'table % is append-only: % is forbidden (history is immutable; insert a new row instead)',
    TG_TABLE_NAME, TG_OP
    USING ERRCODE = 'P0001';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER scientific_object_versions_append_only
  BEFORE UPDATE OR DELETE ON scientific_object_versions
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER relation_versions_append_only
  BEFORE UPDATE OR DELETE ON relation_versions
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER project_states_append_only
  BEFORE UPDATE OR DELETE ON project_states
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER state_commits_append_only
  BEFORE UPDATE OR DELETE ON state_commits
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER releases_append_only
  BEFORE UPDATE OR DELETE ON releases
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER research_asset_versions_append_only
  BEFORE UPDATE OR DELETE ON research_asset_versions
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER asset_lineage_append_only
  BEFORE UPDATE OR DELETE ON asset_lineage
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER policy_versions_append_only
  BEFORE UPDATE OR DELETE ON policy_versions
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER validation_results_append_only
  BEFORE UPDATE OR DELETE ON validation_results
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER contribution_events_append_only
  BEFORE UPDATE OR DELETE ON contribution_events
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER audit_log_append_only
  BEFORE UPDATE OR DELETE ON audit_log
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER research_events_append_only
  BEFORE UPDATE OR DELETE ON research_events
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER external_reference_snapshots_append_only
  BEFORE UPDATE OR DELETE ON external_reference_snapshots
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();
