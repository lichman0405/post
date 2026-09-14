-- +goose Up
-- RSG query surface index (T0209). The query API's candidate scan is
-- project-scoped with the object-type filter as the index's second
-- column; the relations-by-project scan already uses the
-- relations_project_idx created by migration 00025. Performance-only, no
-- semantic content — every shape remains rebuildable from the canonical
-- append-only history.

CREATE INDEX scientific_objects_project_type_idx
  ON scientific_objects (project_id, object_type);

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
