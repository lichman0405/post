-- +goose Up
-- Relations and their versions (canonical lines 146-166).
CREATE TABLE relations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE relation_versions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  relation_id uuid NOT NULL REFERENCES relations(id) ON DELETE RESTRICT,
  version_no integer NOT NULL CHECK(version_no > 0),
  state_id uuid NOT NULL REFERENCES project_states(id) ON DELETE RESTRICT,
  relation_type text NOT NULL,
  source_object_version_id uuid NOT NULL REFERENCES scientific_object_versions(id) ON DELETE RESTRICT,
  target_object_version_id uuid NOT NULL REFERENCES scientific_object_versions(id) ON DELETE RESTRICT,
  payload jsonb NOT NULL DEFAULT '{}'::jsonb,
  integrity_hash text NOT NULL,
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(relation_id,version_no)
);
CREATE INDEX relation_versions_source_idx ON relation_versions(source_object_version_id, relation_type);
CREATE INDEX relation_versions_target_idx ON relation_versions(target_object_version_id, relation_type);
