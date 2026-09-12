-- +goose Up
-- Append-only enforcement, part 2 (follow-up to T0013).
--
-- Row-level triggers do NOT fire on TRUNCATE, so the immutability added in
-- 00014 could still be bypassed wholesale with `TRUNCATE <table> CASCADE`,
-- erasing an entire history in one statement. A statement-level BEFORE
-- TRUNCATE trigger closes that hole, reusing the same append_only_guard()
-- function (which reads TG_TABLE_NAME / TG_OP, set for statement-level
-- triggers exactly as for row-level ones).
--
-- Covers the same 13 tables as 00014. If that set changes, both migrations
-- must be extended with a new forward-only migration - never edited in place.

CREATE TRIGGER asset_lineage_no_truncate
  BEFORE TRUNCATE ON asset_lineage FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER audit_log_no_truncate
  BEFORE TRUNCATE ON audit_log FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER contribution_events_no_truncate
  BEFORE TRUNCATE ON contribution_events FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER external_reference_snapshots_no_truncate
  BEFORE TRUNCATE ON external_reference_snapshots FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER policy_versions_no_truncate
  BEFORE TRUNCATE ON policy_versions FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER project_states_no_truncate
  BEFORE TRUNCATE ON project_states FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER relation_versions_no_truncate
  BEFORE TRUNCATE ON relation_versions FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER releases_no_truncate
  BEFORE TRUNCATE ON releases FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER research_asset_versions_no_truncate
  BEFORE TRUNCATE ON research_asset_versions FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER research_events_no_truncate
  BEFORE TRUNCATE ON research_events FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER scientific_object_versions_no_truncate
  BEFORE TRUNCATE ON scientific_object_versions FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER state_commits_no_truncate
  BEFORE TRUNCATE ON state_commits FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER validation_results_no_truncate
  BEFORE TRUNCATE ON validation_results FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
