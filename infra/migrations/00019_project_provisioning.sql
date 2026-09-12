-- +goose Up
-- Project creation shell (T0104): provision_status records the GitProvider
-- provisioning state. Every new project starts 'pending'; T0301's Gitea
-- adapter moves it to 'provisioned' (or 'failed'). The column is part of
-- the project row, not a separate queue: the provisioning task derives its
-- work list from provision_status = 'pending'.
ALTER TABLE projects ADD COLUMN provision_status text NOT NULL DEFAULT 'pending'
  CHECK (provision_status IN ('pending', 'provisioned', 'failed'));

COMMENT ON COLUMN projects.provision_status IS
  'GitProvider provisioning state: pending (created, repo not provisioned yet), provisioned (T0301), failed';

-- Personal projects (organization_id IS NULL) are not covered by the
-- UNIQUE (organization_id, slug) constraint — PostgreSQL treats NULLs as
-- distinct there — so their slugs need their own uniqueness rule.
CREATE UNIQUE INDEX projects_personal_slug_idx ON projects (slug) WHERE organization_id IS NULL;

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
