-- +goose Up
-- Scientific object version counter (task T0202): current_version_no is both
-- the materialized current head of the append-only version log (docs/21 §5)
-- and the atomic compare-and-swap cell behind expected_version creation. The
-- version rows themselves stay append-only (migrations 00014/00015); this
-- pointer is the one deliberately mutable projection on scientific_objects
-- and is maintained by the repository inside the same transaction that
-- inserts the version row.
ALTER TABLE scientific_objects
  ADD COLUMN current_version_no integer NOT NULL DEFAULT 0
  CHECK (current_version_no >= 0);

-- Backfill from the version log so the pointer matches history on any
-- database where rows were written before this migration.
UPDATE scientific_objects o
   SET current_version_no = v.latest
  FROM (SELECT object_id, max(version_no) AS latest
          FROM scientific_object_versions
         GROUP BY object_id) v
 WHERE v.object_id = o.id;
