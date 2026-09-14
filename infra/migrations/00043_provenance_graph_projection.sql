-- +goose Up
-- Provenance graph projection (task T0505, docs/10): a rebuildable read
-- model of every provenance-category relation version (docs/44: the
-- provenance/structure and lineage/network types), with both endpoint
-- display labels resolved into the row. This is the graph the provenance
-- reads walk: lineage ("where did this come from") and impact ("what
-- depends on this").
--
-- The projection is derived data, not history — unlike the version logs it
-- is deliberately NOT append-only protected (migrations 00014/00015): it
-- can be truncated and rebuilt from canonical truth at any time
-- (rebuild_provenance_edges()). It is maintained incrementally by an AFTER
-- INSERT trigger on relation_versions; because relation_versions is
-- append-only, a projected row is written exactly once and never needs
-- updating or deleting, so INSERT is the only maintenance path.
--
-- The provenance type set is pinned here AND by
-- relationcatalog.ProvenanceTypes() in Go; tests/integration/
-- provenance_test.go seeds one edge per catalog type and asserts projection
-- membership both ways, so the two copies cannot drift silently. The table
-- itself enforces the membership invariant too: the CHECK added after
-- provenance_relation_types() below (the constraint needs the function to
-- exist) keeps any row outside the provenance set out, as defense-in-depth
-- beyond the trigger filter and the rebuild's WHERE clause.
--
-- Additive only: no existing object is altered. The backfill at the end
-- projects the relation versions that already exist when a database
-- upgrades; a fresh install has none and the backfill is a no-op.

CREATE TABLE provenance_edges (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  relation_id uuid NOT NULL REFERENCES relations(id) ON DELETE RESTRICT,
  relation_version_id uuid NOT NULL REFERENCES relation_versions(id) ON DELETE RESTRICT,
  relation_type text NOT NULL,
  source_object_id uuid NOT NULL REFERENCES scientific_objects(id) ON DELETE RESTRICT,
  source_object_version_id uuid NOT NULL REFERENCES scientific_object_versions(id) ON DELETE RESTRICT,
  source_object_type text NOT NULL,
  source_title text NOT NULL,
  source_version_no integer NOT NULL,
  target_object_id uuid NOT NULL REFERENCES scientific_objects(id) ON DELETE RESTRICT,
  target_object_version_id uuid NOT NULL REFERENCES scientific_object_versions(id) ON DELETE RESTRICT,
  target_object_type text NOT NULL,
  target_title text NOT NULL,
  target_version_no integer NOT NULL,
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL,
  UNIQUE(relation_version_id)
);
CREATE INDEX provenance_edges_project_idx ON provenance_edges(project_id, relation_type);
CREATE INDEX provenance_edges_source_version_idx ON provenance_edges(source_object_version_id);
CREATE INDEX provenance_edges_target_version_idx ON provenance_edges(target_object_version_id);

-- provenance_relation_types is the SQL copy of relationcatalog's
-- provenance set (docs/44). Keep it in sync with
-- internal/rsg/relationcatalog ProvenanceInference flags — the integration
-- test's catalog cross-check fails when they diverge.
-- +goose StatementBegin
CREATE FUNCTION provenance_relation_types() RETURNS text[]
LANGUAGE sql IMMUTABLE
AS $$
  SELECT ARRAY[
    -- Provenance/structure (docs/44).
    'contains', 'depends_on', 'derived_from', 'follows_protocol',
    'generated_by', 'parameterized_by', 'part_of', 'performed_on',
    'produces', 'references', 'supersedes', 'uses',
    -- Lineage/network (docs/44).
    'derived_asset_from', 'forked_from', 'originates_from',
    'published_from', 'used_by'
  ];
$$;
-- +goose StatementEnd

-- The projection's own invariant, enforced where the table is born: a
-- row's relation_type must be a provenance type. The trigger filter and
-- the rebuild's WHERE already guarantee it, so this CHECK is
-- defense-in-depth against any future write path — it costs nothing at
-- insert time (the same IMMUTABLE list the trigger consults). It is added
-- after the function because CREATE TABLE cannot reference a function
-- defined later in the migration.
ALTER TABLE provenance_edges
  ADD CONSTRAINT provenance_edges_relation_type_check
  CHECK (relation_type = ANY(provenance_relation_types()));

-- provenance_edges_maintain projects one new relation version. The
-- endpoint joins always match: the relation_versions foreign keys
-- guarantee the rows exist at insert time.
-- +goose StatementBegin
CREATE FUNCTION provenance_edges_maintain() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.relation_type = ANY(provenance_relation_types()) THEN
    INSERT INTO provenance_edges (
      project_id, relation_id, relation_version_id, relation_type,
      source_object_id, source_object_version_id, source_object_type,
      source_title, source_version_no,
      target_object_id, target_object_version_id, target_object_type,
      target_title, target_version_no,
      created_by, created_at)
    SELECT
      r.project_id, NEW.relation_id, NEW.id, NEW.relation_type,
      sov_s.object_id, NEW.source_object_version_id, so_s.object_type,
      sov_s.title, sov_s.version_no,
      sov_t.object_id, NEW.target_object_version_id, so_t.object_type,
      sov_t.title, sov_t.version_no,
      NEW.created_by, NEW.created_at
    FROM relations r
    JOIN scientific_object_versions sov_s ON sov_s.id = NEW.source_object_version_id
    JOIN scientific_objects so_s ON so_s.id = sov_s.object_id
    JOIN scientific_object_versions sov_t ON sov_t.id = NEW.target_object_version_id
    JOIN scientific_objects so_t ON so_t.id = sov_t.object_id
    WHERE r.id = NEW.relation_id;
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER relation_versions_provenance_projection
  AFTER INSERT ON relation_versions
  FOR EACH ROW EXECUTE FUNCTION provenance_edges_maintain();

-- rebuild_provenance_edges() restores the projection from canonical truth
-- (TRUNCATE + one INSERT SELECT). Concurrent writers are safe: the
-- TRUNCATE's ACCESS EXCLUSIVE lock makes the rebuild atomic for its
-- transaction, and rows inserted after it lands through the trigger.
-- +goose StatementBegin
CREATE FUNCTION rebuild_provenance_edges() RETURNS void
LANGUAGE plpgsql
AS $$
BEGIN
  TRUNCATE provenance_edges;
  INSERT INTO provenance_edges (
    project_id, relation_id, relation_version_id, relation_type,
    source_object_id, source_object_version_id, source_object_type,
    source_title, source_version_no,
    target_object_id, target_object_version_id, target_object_type,
    target_title, target_version_no,
    created_by, created_at)
  SELECT
    r.project_id, rv.relation_id, rv.id, rv.relation_type,
    sov_s.object_id, rv.source_object_version_id, so_s.object_type,
    sov_s.title, sov_s.version_no,
    sov_t.object_id, rv.target_object_version_id, so_t.object_type,
    sov_t.title, sov_t.version_no,
    rv.created_by, rv.created_at
  FROM relation_versions rv
  JOIN relations r ON r.id = rv.relation_id
  JOIN scientific_object_versions sov_s ON sov_s.id = rv.source_object_version_id
  JOIN scientific_objects so_s ON so_s.id = sov_s.object_id
  JOIN scientific_object_versions sov_t ON sov_t.id = rv.target_object_version_id
  JOIN scientific_objects so_t ON so_t.id = sov_t.object_id
  WHERE rv.relation_type = ANY(provenance_relation_types());
END;
$$;
-- +goose StatementEnd

-- Backfill: an upgraded database already holds relation versions — project
-- them now so the projection is complete the moment the migration lands.
SELECT rebuild_provenance_edges();

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
