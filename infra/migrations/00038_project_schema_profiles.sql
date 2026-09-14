-- +goose Up
-- T0213 project schema profiles (docs/21 §8): a project registers namespaced,
-- versioned JSON Schema profiles that extend an official base schema with
-- project-specific typed fields — custom metadata never requires changing
-- platform core. Profile versions are immutable: an id+version pair, once
-- registered, is never overwritten or deleted (the append_only_guard trigger
-- below); new content takes a new version. Every scientific object version
-- pins its (schema_id, schema_version), so a profile v2 never invalidates
-- history written under v1 (docs/21 §8: old schema data always stays valid).
--
-- schema_id is the profile's namespaced registry id, the form
-- "project:<project_id>:<name>" — derived server-side, so two projects can
-- never squat the same id and no profile can squat a canonical id (the
-- registry refuses the canonical namespace at registration too).
CREATE TABLE project_schema_profiles (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  schema_id text NOT NULL,
  version text NOT NULL,
  -- The official base schema this profile extends, pinned by id+version:
  -- which canonical type the profile governs is a stored fact, not a guess.
  base_schema_id text NOT NULL,
  base_schema_version text NOT NULL,
  -- The exact generated profile document bytes (Go-canonical JSON). TEXT,
  -- not jsonb: the content hash must pin the exact bytes the registry
  -- registered, and jsonb normalization would break byte equality.
  content text NOT NULL,
  content_hash text NOT NULL,
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (project_id, schema_id, version),
  CONSTRAINT project_schema_profiles_schema_id_check
    CHECK (schema_id LIKE '%:%' AND char_length(schema_id) <= 512),
  CONSTRAINT project_schema_profiles_version_check
    CHECK (version ~ '^[A-Za-z0-9._-]{1,64}$'),
  CONSTRAINT project_schema_profiles_base_check
    CHECK (char_length(base_schema_id) BETWEEN 1 AND 512
           AND char_length(base_schema_version) BETWEEN 1 AND 64),
  CONSTRAINT project_schema_profiles_content_check
    CHECK (jsonb_typeof(content::jsonb) = 'object'),
  CONSTRAINT project_schema_profiles_hash_check
    CHECK (content_hash ~ '^[0-9a-f]{64}$')
);

CREATE INDEX project_schema_profiles_project_idx
  ON project_schema_profiles (project_id, schema_id, created_at DESC, id DESC);

CREATE TRIGGER project_schema_profiles_append_only
  BEFORE UPDATE OR DELETE ON project_schema_profiles
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

-- Row-level triggers do not fire on TRUNCATE (00015): the immutability above
-- could still be bypassed wholesale with `TRUNCATE ... CASCADE`. The
-- statement-level half reuses the same append_only_guard() function.
CREATE TRIGGER project_schema_profiles_no_truncate
  BEFORE TRUNCATE ON project_schema_profiles FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
