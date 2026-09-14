-- +goose Up
-- GitProvider repository provisioning (T0301): the canonical record of the
-- platform→GitProvider (Gitea) repository mapping and the push-webhook HMAC
-- secret.
--
-- projects.git_repository_external_id (00003) is filled with the
-- GitProvider-side repository reference ("<owner>/<name>") once the
-- repository exists; the CHECK keeps the row honest — a project can never
-- report provisioned without the external reference.
--
-- Deliberately NOT added to projects: the failure reason. The canonical
-- store records the STATE (provision_status = 'failed', 00019) and the
-- job loop's structured logs carry the redacted reason — a projects column
-- would drag every project read path into the provisioning concern (and
-- sqlc's project queries select whole rows).
--
-- The webhook secret is also NOT a projects column: it is a credential
-- (docs/55 SECRET class), so it lives in its own table, is read only by
-- internal/gitprovider's own queries, and never rides the project
-- read/write paths or any API payload. One row per project IS the
-- project→repo 1:1 invariant at the storage layer (primary key on
-- project_id), with UNIQUE (owner, name) backing the name-derivation rule.
ALTER TABLE projects ADD CONSTRAINT projects_provision_consistent
  CHECK (provision_status <> 'provisioned' OR git_repository_external_id IS NOT NULL);

CREATE TABLE git_repository_provisions (
  project_id uuid PRIMARY KEY REFERENCES projects(id) ON DELETE RESTRICT,
  owner text NOT NULL,
  name text NOT NULL,
  gitea_repo_id bigint NOT NULL,
  webhook_id bigint NOT NULL,
  webhook_secret text NOT NULL,
  provisioned_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (owner, name)
);

COMMENT ON TABLE git_repository_provisions IS
  'The platform→GitProvider repository mapping (T0301): one row per provisioned project (project→repo 1:1). Product domain objects never read Gitea state; this table is the platform-side record of the provider-side facts.';

COMMENT ON COLUMN git_repository_provisions.webhook_secret IS
  'HMAC secret of the push webhook (docs/55 SECRET). Written at provisioning, read only by internal/gitprovider (T0305 push verification); never exposed through the API.';

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
