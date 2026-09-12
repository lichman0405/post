-- +goose Up
-- Projects, programs, memberships, policy versions (canonical lines 34-77).
CREATE TABLE programs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid REFERENCES organizations(id) ON DELETE RESTRICT,
  slug text NOT NULL,
  name text NOT NULL,
  description text,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (organization_id, slug)
);

CREATE TABLE projects (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid REFERENCES organizations(id) ON DELETE RESTRICT,
  program_id uuid REFERENCES programs(id) ON DELETE SET NULL,
  slug text NOT NULL,
  name text NOT NULL,
  purpose text NOT NULL,
  activity_status text NOT NULL DEFAULT 'planning' CHECK (activity_status IN ('planning','active','paused','archived')),
  visibility text NOT NULL CHECK (visibility IN ('public','private')),
  main_frozen boolean NOT NULL DEFAULT false,
  git_repository_external_id text,
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (organization_id,slug)
);

CREATE TABLE project_memberships (
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  role text NOT NULL CHECK (role IN ('owner','maintainer','contributor','viewer')),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(project_id,user_id)
);

CREATE TABLE policy_versions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid REFERENCES organizations(id) ON DELETE RESTRICT,
  project_id uuid REFERENCES projects(id) ON DELETE RESTRICT,
  version text NOT NULL,
  policy_json jsonb NOT NULL,
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK ((organization_id IS NOT NULL) <> (project_id IS NOT NULL))
);
