-- +goose Up
-- The research-event half of the Project Activity feed (T0607).
--
-- The Activity page reads two registries as ONE key-set-paginated sequence
-- on (occurred_at, id): the project's governance rows in audit_log and its
-- research events in research_events (docs/26 §1 keeps the two apart —
-- the page reads both, it does not merge them). The audit half already has
-- the index that makes that read a range scan:
--
--   audit_log_project_occurred_idx (project_id, occurred_at DESC, id DESC)  -- 00020
--
-- research_events had no index at all beyond its primary key. Its only
-- readers so far ask by identity — the asset page joins on
-- payload->>'asset_id', the webhook and notification fan-outs join on
-- outbox_event_id or deliver by id — so every one of them is served by a
-- key or a join, and the table has been small enough that nobody noticed.
-- The Activity feed asks the question those readers never ask: "this
-- project's events, newest first, from this cursor". Without an index that
-- is a sequential scan of every event on the platform per page, and it is
-- the only read in the feed whose cost grows with the platform rather than
-- with the project.
--
-- The column order mirrors the audit index exactly, because the queries
-- are the same query: (project_id) is the scope predicate, and the
-- (occurred_at DESC, id DESC) suffix is the ordering the keyset cursor
-- rides, so one index serves the ORDER BY and the
-- `(occurred_at, id) < (before_ts, before_id)` predicate at once.
--
-- DESC on both trailing columns is what makes the page a forward walk
-- rather than a sort: the feed is always read newest-first, and a
-- backwards index would have PostgreSQL read the whole project's history
-- to answer the first page. NULLS are not a consideration — both columns
-- are NOT NULL (00012).
--
-- project_id is nullable in research_events (an event need not be
-- project-scoped), which is why this is a plain b-tree and not a partial
-- index: the feed's scope predicate is equality on a non-null id, so the
-- NULL rows are simply outside every range this index is asked for, and
-- excluding them from the index would buy nothing the range already does.
CREATE INDEX research_events_project_occurred_idx
  ON research_events (project_id, occurred_at DESC, id DESC);

COMMENT ON INDEX research_events_project_occurred_idx IS
  'The project Activity feed''s research-event branch (T0607): the project''s research events newest-first, the shape the (occurred_at, id) keyset cursor of internal/persistence.ListProjectActivity rides. Mirrors audit_log_project_occurred_idx (00020) so both halves of the merged feed are range scans.';
