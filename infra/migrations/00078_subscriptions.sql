-- +goose Up
-- Subscription model (T1002, docs/18 §3): a user follows a target —
-- Project / Research Asset / Published Knowledge Object / Person /
-- Organization — and chooses which event types reach them and through
-- which channels (docs/18 §4). The table itself predates this migration
-- as a 00012 stub with no vocabulary and no lifetime; this migration
-- turns it into a model and adds the two tables the fan-out needs.
--
-- Three pieces land here:
--
--   1. subscriptions: the target/channel vocabulary as CHECKs, a live-row
--      uniqueness rule, and the soft-delete marker unsubscribing stamps.
--      The channels CHECK is a subset test, not an equality test: the
--      column's own default is '{web}' and a subscriber may add 'email'
--      later without a migration. Every write path also goes through
--      events.ValidateChannels in Go — the CHECK is the second line, for
--      a session that writes SQL directly (the same split 00059 uses for
--      the webhook registry).
--
--      Unsubscribe is a soft delete, not a row removal: the delivery rows
--      a past subscription produced must keep naming it (ON DELETE
--      RESTRICT on subscription_deliveries.subscription_id would refuse a
--      hard delete anyway), and a re-subscribe must be distinguishable
--      from a subscription that never ended. The partial unique index is
--      what makes "one live subscription per (user, target)" true: it
--      covers only rows the user has not unsubscribed from, so the
--      history of ended subscriptions is unbounded while the live set
--      stays unique.
--
--   2. subscription_deliveries: one row per (subscription, event,
--      channel). It is a POINTER, not a copy: event identity, target
--      identity and event type, never event content. Rendering the
--      content is a read of the append-only research event under the
--      renderer's own gate, exactly as webhook_deliveries pins event_type
--      so its log reads without a join. The partial unique index is the
--      fan-out's idempotency guarantee — a retried fan-out re-inserts the
--      same (subscription, event, channel) triple and hits DO NOTHING —
--      the same shape webhook_deliveries_endpoint_event_uniq gives the
--      webhook pipeline (00059).
--
--      Status: 'web' rows are born 'delivered' — the research inbox IS the
--      delivery, so there is nothing left to do (T1003 owns read/unread on
--      top of them) — while 'email' rows are born 'pending' for the
--      digest sender that consumes them (T1005). 'cancelled' is what
--      unsubscribing stamps on the rows still in flight, and what the
--      fan-out stamps when it finds a subscription whose owner can no
--      longer see the target.
--
--   3. subscription_fanned_events: the fan-out's exactly-once cursor, one
--      row per outbox row the subscription fan-out has consumed. The
--      webhook pipeline keeps the same cursor as a column on
--      outbox_events (00059's webhook_fanned_out_at); this one is its own
--      table because outbox_events is modeled by checked-in sqlc queries
--      (internal/persistence/queries/events_audit.sql: SELECT * FROM
--      outbox_events, INSERT ... RETURNING *), so a new column there
--      moves generated code outside this task's scope. The semantics are
--      identical: the cursor row is written in the same transaction that
--      writes the deliveries, so a crash between them re-runs the
--      idempotent fan-out instead of losing the event, and each consumer
--      marks its own progress without ordering assumptions about what the
--      publisher has published so far.

ALTER TABLE subscriptions
  ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now(),
  ADD COLUMN deleted_at timestamptz;

ALTER TABLE subscriptions
  ADD CONSTRAINT subscriptions_target_type_valid
    CHECK (target_type IN ('project', 'asset', 'knowledge', 'user', 'organization')),
  ADD CONSTRAINT subscriptions_target_id_present
    CHECK (target_id <> ''),
  ADD CONSTRAINT subscriptions_channels_valid
    CHECK (cardinality(channels) > 0 AND channels <@ ARRAY['web', 'email']::text[]);

-- The target id's shape per type: a uuid for the four targets the
-- resolution queries cast, the asset pid for an asset (the same regexp
-- research_assets_pid_format enforces on the pid column, migration 00064).
-- events.ValidateTargetID checks the identical shapes on the application
-- path; this is the second line, for a session that writes SQL directly —
-- and it is what makes the uuids above castable inside a query instead of
-- a runtime error the fan-out would have to survive.
ALTER TABLE subscriptions
  ADD CONSTRAINT subscriptions_target_id_shape
    CHECK (CASE target_type
             WHEN 'asset' THEN target_id ~ '^[0-9a-hjkmnp-tv-z]{26}$'
             ELSE target_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
           END);

COMMENT ON TABLE subscriptions IS
  'One user following one target (T1002, docs/18 §3). target_type is the canonical target vocabulary; target_id is the target''s stable identity — a uuid for project/knowledge/user/organization, the asset pid for asset (the same identifier /assets/{pid} is built from). deleted_at is the unsubscribe marker: a deleted subscription matches no fan-out, and its in-flight deliveries are cancelled in the same transaction.';

COMMENT ON COLUMN subscriptions.event_filters IS
  'Event types this subscription delivers, exact match (events.ValidateEventFilters); empty means every event the target addresses. The canonical vocabulary is specs/events/event-types.yaml — it is not duplicated here or in code, so an unknown type simply never matches.';

COMMENT ON COLUMN subscriptions.channels IS
  'Output interfaces this subscription delivers through (docs/18 §4): web = the research inbox (a delivered delivery row, T1003), email = the digest sender''s queue (a pending delivery row, T1005). A non-empty subset of the vocabulary.';

COMMENT ON COLUMN subscriptions.deleted_at IS
  'soft-delete marker: set when the owner unsubscribes; NULL = live. Nothing disappears (CLAUDE.md §9.8) — the row keeps its id so the deliveries it produced still name it, and the live-row unique index lets the same (user, target) be followed again as a NEW row.';

-- One live subscription per (user, target). Ended subscriptions are not
-- covered, so a re-subscribe after an unsubscribe inserts a new row.
CREATE UNIQUE INDEX subscriptions_live_uniq
  ON subscriptions (user_id, target_type, target_id)
  WHERE deleted_at IS NULL;

-- The fan-out's candidate scan: live subscriptions for one target. The
-- lookup is by target, so the index leads with the target columns.
CREATE INDEX subscriptions_live_target_idx
  ON subscriptions (target_type, target_id)
  WHERE deleted_at IS NULL;

CREATE TABLE subscription_deliveries (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  subscription_id uuid NOT NULL REFERENCES subscriptions(id) ON DELETE RESTRICT,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  event_id uuid NOT NULL REFERENCES research_events(id) ON DELETE RESTRICT,
  channel text NOT NULL CHECK (channel IN ('web', 'email')),
  event_type text NOT NULL,
  target_type text NOT NULL,
  target_id text NOT NULL,
  status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'delivered', 'cancelled')),
  created_at timestamptz NOT NULL DEFAULT now(),
  delivered_at timestamptz,
  cancelled_at timestamptz,
  CHECK ((status = 'delivered') = (delivered_at IS NOT NULL)),
  CHECK ((status = 'cancelled') = (cancelled_at IS NOT NULL))
);

COMMENT ON TABLE subscription_deliveries IS
  'One notification per (subscription, event, channel): the pointer the research inbox (T1003) and the digest sender (T1005) read. It carries NO event content — event_id names the append-only research event and target_type/target_id name the subject, so rendering is a read under the renderer''s own current-authorization gate. The partial unique index makes the fan-out idempotent.';

COMMENT ON COLUMN subscription_deliveries.status IS
  'pending = in flight for a channel that has a sender (email); delivered = delivered (the web channel is delivered the moment the row exists — the inbox row IS the delivery, and read/unread on top of it is T1003); cancelled = withdrawn before delivery, by the owner unsubscribing or by the fan-out finding the subscriber can no longer see the target.';

COMMENT ON COLUMN subscription_deliveries.user_id IS
  'The subscriber the row was fanned out for, denormalized from the subscription so the inbox reads one table and so a cancelled-then-re-followed target cannot re-attribute an old row to a new subscription.';

COMMENT ON COLUMN subscription_deliveries.event_type IS
  'The fanned-out event''s type, pinned at fan-out (the delivery must read without a join into the append-only event log — the same reason webhook_deliveries pins it).';

COMMENT ON COLUMN subscription_deliveries.cancelled_at IS
  'When the row was cancelled. Set together with status = cancelled (the CHECK pairs them), so a withdrawn notification is auditable rather than merely gone.';

-- The fan-out's idempotency guarantee: retried fan-outs hit DO NOTHING.
CREATE UNIQUE INDEX subscription_deliveries_uniq
  ON subscription_deliveries (subscription_id, event_id, channel);

-- The inbox read (T1003): one subscriber's deliveries, newest first.
CREATE INDEX subscription_deliveries_inbox_idx
  ON subscription_deliveries (user_id, status, created_at DESC, id DESC);

-- Unsubscribe and revocation cancel in-flight rows of one subscription.
CREATE INDEX subscription_deliveries_pending_idx
  ON subscription_deliveries (subscription_id)
  WHERE status = 'pending';

CREATE TABLE subscription_fanned_events (
  outbox_event_id uuid PRIMARY KEY REFERENCES outbox_events(id) ON DELETE RESTRICT,
  fanned_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE subscription_fanned_events IS
  'The subscription fan-out''s exactly-once cursor (T1002): one row per outbox row the fan-out has consumed, written in the same transaction as that row''s delivery inserts, so a crash between the two re-runs an idempotent fan-out instead of losing an event. It lives in its own table rather than as a column on outbox_events because outbox_events is modeled by checked-in sqlc queries (SELECT * / RETURNING *), which a new column would move.';

-- +goose Down
DROP TABLE subscription_fanned_events;
DROP INDEX subscription_deliveries_pending_idx;
DROP INDEX subscription_deliveries_inbox_idx;
DROP INDEX subscription_deliveries_uniq;
DROP TABLE subscription_deliveries;
DROP INDEX subscriptions_live_target_idx;
DROP INDEX subscriptions_live_uniq;
ALTER TABLE subscriptions
  DROP CONSTRAINT subscriptions_target_id_shape,
  DROP CONSTRAINT subscriptions_channels_valid,
  DROP CONSTRAINT subscriptions_target_id_present,
  DROP CONSTRAINT subscriptions_target_type_valid,
  DROP COLUMN deleted_at,
  DROP COLUMN updated_at;
