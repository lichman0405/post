-- +goose Up
-- Transactional outbox publish (T1001): the domain writes outbox_events in
-- the same transaction as its state change (docs/53, ADR-013), and the
-- worker publishes each pending row into research_events — the append-only
-- domain event log (00014-guarded). Two gaps close here:
--
--   1. Envelope columns: the published research_event needs actor_id,
--      project_id and visibility, and the publisher must copy them from
--      the outbox row, never re-derive them from the payload (a private
--      event must not be publishable as a public one). event_type,
--      correlation_id and payload exist since 00012. visibility defaults
--      to 'private' — fail closed — so rows written before this migration
--      publish at the least visible level, and an explicit value is the
--      only way an event becomes public.
--
--   2. Idempotent publish (the acceptance criterion: crash/retry
--      不丢事件不重复副作用): the publisher inserts the research_event
--      and marks the outbox row published in ONE transaction, and a crash
--      between the two must not produce a second research_event on retry.
--      research_events.outbox_event_id pins the outbox row each event came
--      from, and the partial unique index below makes the retried insert a
--      no-op (INSERT ... ON CONFLICT DO NOTHING). The index is partial
--      because direct research_events writes (no outbox row) stay legal:
--      NULL is never a conflict, and outbox-published rows are always
--      non-NULL.
--
--   3. Failure record: last_error carries why the last publish attempt
--      failed. Rows are retried forever — no event may be silently lost
--      (docs/26 §2 alerts on outbox backlog) — and the recorded error is
--      how an operator sees a stuck row.

ALTER TABLE outbox_events
  ADD COLUMN actor_id uuid REFERENCES users(id) ON DELETE RESTRICT,
  ADD COLUMN project_id uuid REFERENCES projects(id) ON DELETE RESTRICT,
  ADD COLUMN visibility text NOT NULL DEFAULT 'private',
  ADD COLUMN last_error text;

COMMENT ON COLUMN outbox_events.visibility IS
  'The event''s visibility preset, copied verbatim into the published research_event: public or private (docs/12 — an event is never more visible than its subject). ''private'' is the fail-closed default for rows written before this migration; the outbox recorder always sets an explicit value.';

COMMENT ON COLUMN outbox_events.last_error IS
  'Why the last publish attempt failed, when it did (T1001): a row whose publish fails is retried forever, and this column is the operator''s window onto a stuck row (docs/26 §2 outbox backlog alerting). NULL while never attempted or after a successful publish.';

ALTER TABLE research_events
  ADD COLUMN outbox_event_id uuid REFERENCES outbox_events(id) ON DELETE RESTRICT;

COMMENT ON COLUMN research_events.outbox_event_id IS
  'The outbox row this event was published from (T1001). The partial unique index research_events_outbox_event_uniq makes the publish step idempotent: a retried publish after a crash between the insert and the published-mark is a no-op. NULL for research events written directly, bypassing the outbox.';

-- The publish dedupe: one research_event per outbox row, ever.
CREATE UNIQUE INDEX research_events_outbox_event_uniq
  ON research_events (outbox_event_id)
  WHERE outbox_event_id IS NOT NULL;

-- The publisher's backlog scan (WHERE published_at IS NULL, ordered by
-- created_at, id): only pending rows enter the index, so the scan stays
-- small no matter how long the published history grows.
CREATE INDEX outbox_events_pending_idx
  ON outbox_events (created_at, id)
  WHERE published_at IS NULL;

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
