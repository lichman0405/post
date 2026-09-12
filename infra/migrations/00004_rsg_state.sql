-- +goose Up
-- RSG state model: branches, project states, state commits
-- (canonical lines 79-117).
--
-- NOTE: creation order differs from the canonical seed file: `branches` is
-- created before `project_states` because project_states references
-- branches(id) inline and PostgreSQL cannot create a table that references a
-- not-yet-existing relation. All constraints are preserved verbatim; only the
-- statement order was made executable. branches.base_state_id is still added
-- as a separate FK constraint via ALTER TABLE, exactly as in the canonical
-- seed (lines 104).
CREATE TABLE branches (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  name text NOT NULL,
  visibility text NOT NULL CHECK (visibility IN ('public','private')),
  purpose text,
  git_ref text NOT NULL,
  base_state_id uuid,
  lifecycle_state text NOT NULL DEFAULT 'active' CHECK (lifecycle_state IN ('active','merged','aborted')),
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(project_id,name)
);

CREATE TABLE project_states (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  branch_id uuid REFERENCES branches(id) ON DELETE RESTRICT,
  parent_state_id uuid REFERENCES project_states(id) ON DELETE RESTRICT,
  state_hash text NOT NULL,
  git_commit_sha text,
  manifest_version text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(project_id,state_hash)
);
ALTER TABLE branches ADD CONSTRAINT branches_base_state_fk FOREIGN KEY(base_state_id) REFERENCES project_states(id) ON DELETE RESTRICT;

CREATE TABLE state_commits (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  branch_id uuid NOT NULL REFERENCES branches(id) ON DELETE RESTRICT,
  base_state_id uuid REFERENCES project_states(id) ON DELETE RESTRICT,
  result_state_id uuid NOT NULL REFERENCES project_states(id) ON DELETE RESTRICT,
  actor_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  via text NOT NULL CHECK (via IN ('web','api','mcp','claude_code','git_compat','system')),
  message text NOT NULL,
  operation_summary jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
