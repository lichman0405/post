-- +goose Up
-- T0214 official project templates: the instantiation record. Creating a
-- project from an official template applies the template's DEFAULTS once
-- — project purpose/visibility, schema profile registrations, the first
-- project policy version, and the initial research-map questions. A
-- template never controls the project afterwards: everything applied is
-- an ordinary project-owned row (schema profile versions, policy
-- versions, scientific objects) the owner evolves through the normal
-- surfaces, and a template upgrade is a new template version that only
-- affects FUTURE instantiations.
--
-- This table records the provenance fact: which template id + version a
-- project was created from (the requirement "记录 template id/version").
-- One project has at most one template origin — UNIQUE (project_id). The
-- template catalog itself is platform code (official templates are not
-- user data), so template_id is a plain label, not a foreign key.
--
-- Rows are append-only provenance (the guard triggers below): the fact
-- that project X came from template Y vZ never changes and never
-- disappears, even if the catalog later retires that template version.
CREATE TABLE project_template_instantiations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  template_id text NOT NULL,
  template_version text NOT NULL,
  -- The template's display name at instantiation time, so the record is
  -- readable without consulting the catalog history of that version.
  template_name text NOT NULL,
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (project_id),
  CONSTRAINT project_template_instantiations_template_id_check
    CHECK (template_id ~ '^[a-z0-9][a-z0-9-]{0,63}$'),
  CONSTRAINT project_template_instantiations_template_version_check
    CHECK (template_version ~ '^[A-Za-z0-9._-]{1,64}$'),
  CONSTRAINT project_template_instantiations_template_name_check
    CHECK (char_length(template_name) BETWEEN 1 AND 200)
);

CREATE INDEX project_template_instantiations_template_idx
  ON project_template_instantiations (template_id, template_version);

CREATE TRIGGER project_template_instantiations_append_only
  BEFORE UPDATE OR DELETE ON project_template_instantiations
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

-- Row-level triggers do not fire on TRUNCATE (00015): the immutability above
-- could still be bypassed wholesale with `TRUNCATE ... CASCADE`. The
-- statement-level half reuses the same append_only_guard() function.
CREATE TRIGGER project_template_instantiations_no_truncate
  BEFORE TRUNCATE ON project_template_instantiations FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
