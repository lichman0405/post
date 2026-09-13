-- +goose Up
-- Audit log organization scope (T0110): governance events (org.created,
-- org.updated, org.deactivated, org.member.*) are organization-scoped, and
-- the activity query filters by scope. audit_log already carries project_id;
-- the new nullable organization_id gives the organization scope the same
-- shape. The column is appended (ALTER TABLE ADD COLUMN), so it sits after
-- occurred_at in ordinal order.
--
-- Append-only enforcement already covers audit_log (00014 row triggers for
-- UPDATE/DELETE, 00015 statement-level TRUNCATE guard): new rows only, and
-- this column is written once at INSERT like every other column.
ALTER TABLE audit_log ADD COLUMN organization_id uuid REFERENCES organizations(id) ON DELETE RESTRICT;

-- Activity queries walk the log newest-first per scope; the composite
-- indexes order by (scope, occurred_at, id) so the keyset page
-- (occurred_at, id) < (before_ts, before_id) rides one index per scope.
CREATE INDEX audit_log_project_occurred_idx ON audit_log (project_id, occurred_at DESC, id DESC);
CREATE INDEX audit_log_organization_occurred_idx ON audit_log (organization_id, occurred_at DESC, id DESC);
CREATE INDEX audit_log_actor_occurred_idx ON audit_log (actor_id, occurred_at DESC, id DESC);

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
