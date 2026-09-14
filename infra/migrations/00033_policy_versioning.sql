-- +goose Up
-- T0603 policy versioning (docs/12 §5): per-scope version uniqueness and
-- the version shape guard. policy_versions itself is append-only
-- (00014/00015 reject UPDATE/DELETE/TRUNCATE at the database): a policy
-- never mutates in place, a change is always a NEW version row, so old
-- versions stay queryable by construction. Each scope line now also has
-- a unique version string — a version number is never reused, not even
-- across a gap.
CREATE UNIQUE INDEX policy_versions_org_version_idx
  ON policy_versions (organization_id, version)
  WHERE organization_id IS NOT NULL;

CREATE UNIQUE INDEX policy_versions_project_version_idx
  ON policy_versions (project_id, version)
  WHERE project_id IS NOT NULL;

-- The version string is a required, bounded label (the app layer allows
-- [A-Za-z0-9._-], this guard only enforces the shape that belongs in the
-- database itself).
ALTER TABLE policy_versions
  ADD CONSTRAINT policy_versions_version_check
  CHECK (char_length(version) BETWEEN 1 AND 64);

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
