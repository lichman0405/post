-- +goose Up
-- Relation version counter (task T0203): current_version_no is both the
-- materialized current head of the append-only relation_versions log
-- (docs/21 §5) and the atomic compare-and-swap cell behind expected_version
-- creation, exactly as 00024 does for scientific objects (T0202). The
-- version rows themselves stay append-only (migrations 00014/00015); this
-- pointer is the one deliberately mutable projection on relations and is
-- maintained by the repository inside the same transaction that inserts the
-- version row.
ALTER TABLE relations
  ADD COLUMN current_version_no integer NOT NULL DEFAULT 0
  CHECK (current_version_no >= 0);

-- Backfill from the version log so the pointer matches history on any
-- database where rows were written before this migration.
UPDATE relations r
   SET current_version_no = v.latest
  FROM (SELECT relation_id, max(version_no) AS latest
          FROM relation_versions
         GROUP BY relation_id) v
 WHERE v.relation_id = r.id;

-- The query-by-type paths (relations of a project filtered by relation
-- type) join relation_versions to relations on the project boundary;
-- relations.project_id had no index (the canonical seed indexes only the
-- relation_versions endpoints).
CREATE INDEX relations_project_idx ON relations(project_id);
