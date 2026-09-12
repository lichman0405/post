-- +goose Up
-- Organization governance (T0103): organizations.deactivated_at is the
-- only "delete" the domain offers (CLAUDE.md §9.8 — nothing disappears,
-- state only evolves). History keeps referring to deactivated organizations;
-- governance refuses new memberships/role changes on them (enforced by the
-- application layer, checked by tests).
ALTER TABLE organizations ADD COLUMN deactivated_at timestamptz NULL;

COMMENT ON COLUMN organizations.deactivated_at IS
  'soft-delete marker: set when the organization is deactivated by its owner; NULL = active';

-- The membership listing hot path (a user''s organizations) filters and
-- joins on user_id, which the composite PK does not cover from this side.
CREATE INDEX organization_memberships_user_idx ON organization_memberships (user_id);

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
