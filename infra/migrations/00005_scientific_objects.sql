-- +goose Up
-- Scientific objects and their append-only versions (canonical lines 119-144).
CREATE TABLE scientific_objects (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  object_type text NOT NULL,
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE scientific_object_versions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  object_id uuid NOT NULL REFERENCES scientific_objects(id) ON DELETE RESTRICT,
  version_no integer NOT NULL CHECK(version_no > 0),
  state_id uuid NOT NULL REFERENCES project_states(id) ON DELETE RESTRICT,
  branch_id uuid REFERENCES branches(id) ON DELETE RESTRICT,
  schema_id text NOT NULL,
  schema_version text NOT NULL,
  title text NOT NULL,
  lifecycle_state text NOT NULL CHECK (lifecycle_state IN ('active','aborted','reopened','superseded')),
  payload jsonb NOT NULL,
  visibility_policy_id uuid,
  integrity_hash text NOT NULL,
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(object_id,version_no)
);
CREATE INDEX scientific_object_versions_payload_gin ON scientific_object_versions USING gin(payload);
