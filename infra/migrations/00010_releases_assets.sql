-- +goose Up
-- Validation results, releases, research assets, publications
-- (canonical lines 244-312).
CREATE TABLE validation_results (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  state_id uuid NOT NULL REFERENCES project_states(id) ON DELETE RESTRICT,
  gate text NOT NULL CHECK(gate IN ('pr','main','release','asset')),
  status text NOT NULL CHECK(status IN ('passed','failed','warning')),
  result_json jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE releases (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  version text NOT NULL,
  title text NOT NULL,
  state_id uuid NOT NULL REFERENCES project_states(id) ON DELETE RESTRICT,
  policy_version_id uuid REFERENCES policy_versions(id) ON DELETE RESTRICT,
  manifest jsonb NOT NULL,
  manifest_hash text NOT NULL,
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(project_id,version)
);

CREATE TABLE research_assets (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  asset_type text NOT NULL CHECK(asset_type IN ('dataset','protocol','material_collection','benchmark')),
  slug text NOT NULL,
  title text NOT NULL,
  origin_project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE research_asset_versions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  asset_id uuid NOT NULL REFERENCES research_assets(id) ON DELETE RESTRICT,
  version text NOT NULL,
  source_release_id uuid REFERENCES releases(id) ON DELETE RESTRICT,
  manifest jsonb NOT NULL,
  rights_json jsonb NOT NULL,
  visibility text NOT NULL CHECK(visibility IN ('public','private')),
  integrity_hash text NOT NULL,
  published_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  published_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(asset_id,version)
);
CREATE TABLE asset_lineage (
  parent_asset_version_id uuid NOT NULL REFERENCES research_asset_versions(id) ON DELETE RESTRICT,
  child_asset_version_id uuid NOT NULL REFERENCES research_asset_versions(id) ON DELETE RESTRICT,
  relation_type text NOT NULL CHECK(relation_type IN ('forked_from','derived_from','supersedes')),
  PRIMARY KEY(parent_asset_version_id,child_asset_version_id,relation_type)
);
CREATE TABLE asset_dependencies (
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  asset_version_id uuid NOT NULL REFERENCES research_asset_versions(id) ON DELETE RESTRICT,
  dependency_type text NOT NULL,
  visibility_of_usage text NOT NULL DEFAULT 'private' CHECK(visibility_of_usage IN ('public','private')),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(project_id,asset_version_id,dependency_type)
);

CREATE TABLE knowledge_publications (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  object_version_id uuid NOT NULL REFERENCES scientific_object_versions(id) ON DELETE RESTRICT,
  public_version text NOT NULL,
  rights_json jsonb NOT NULL,
  published_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  published_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(object_version_id,public_version)
);
