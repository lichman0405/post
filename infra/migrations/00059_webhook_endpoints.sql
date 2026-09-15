-- +goose Up
-- Signed webhooks (T1006, docs/18 §4, docs/22 §9): user-registered
-- delivery endpoints that receive signed research events. The pipeline is
-- outbox (00012) -> research_events (dispatcher, T1001) -> per-endpoint
-- fan-out (this migration's cursor on outbox_events) -> signed HTTP
-- delivery with retry (worker Deliverer).
--
-- Three pieces land here:
--
--   1. webhook_endpoints: the endpoint registry. Each endpoint carries its
--      own HMAC secret, generated at creation, returned to the owner ONCE
--      and never again (any API read omits it by construction). The
--      secret is stored as plaintext because the deliverer must sign with
--      it — a hash would make signing impossible (unlike PATs, which are
--      compared, not used as keys). The consecutive_failures counter and
--      enabled flag drive the disable policy: after
--      DefaultConsecutiveFailureLimit failed deliveries the deliverer
--      flips enabled off; the owner may re-enable it.
--
--   2. outbox_events.webhook_fanned_out_at: the fan-out cursor. A row is
--      fanned out exactly once — the deliverer claims published-but-not-
--      fanned rows (FOR UPDATE SKIP LOCKED, the T1001 pattern) and marks
--      them in the same transaction as the delivery inserts, so a crash
--      between insert and mark re-runs the idempotent fan-out (the unique
--      index in 3. makes it a no-op). The cursor lives on the outbox row
--      instead of a watermark on research_events because it needs no
--      ordering assumption: whatever the publisher has published, in
--      whatever order, is fanned out in published order.
--
--   3. webhook_deliveries delivery-log columns: endpoint_id pins which
--      registered endpoint the delivery targets (SET NULL, not CASCADE:
--      even a physical removal keeps the delivery log rows, and the raw
--      endpoint column preserves the fan-out-time URL snapshot for the
--      log — the deliverer posts to the snapshot, so the log always says
--      where the request actually went), event_type is the delivered
--      event's type at fan-out, next_retry_at schedules the retry (NULL +
--      status 'pending' = due now; the deliverer also uses it as the
--      in-flight lease), last_error/delivered_at complete the log. The
--      partial unique index (endpoint_id, event_id) is the fan-out
--      idempotency guarantee: one delivery row per endpoint per event,
--      ever.

CREATE TABLE webhook_endpoints (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  url text NOT NULL,
  secret text NOT NULL,
  event_filters text[] NOT NULL DEFAULT '{}',
  enabled boolean NOT NULL DEFAULT true,
  consecutive_failures integer NOT NULL DEFAULT 0,
  disabled_at timestamptz,
  deleted_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK (url <> ''),
  CHECK (secret <> '')
);

COMMENT ON TABLE webhook_endpoints IS
  'User-registered webhook delivery endpoints (T1006). One HMAC secret per endpoint, returned to the owner only at creation/rotation; enabled is the disable-policy switch (deliverer flips it after consecutive failures, the owner may re-enable).';

COMMENT ON COLUMN webhook_endpoints.url IS
  'Delivery target, validated as http(s) with a host and no embedded credentials at the service layer. The delivery-log snapshot in webhook_deliveries.endpoint is what the deliverer actually posts to.';

COMMENT ON COLUMN webhook_endpoints.secret IS
  'HMAC-SHA256 signing secret (generated, 32 random bytes hex-encoded). Plaintext by necessity — the deliverer signs with it; a stored hash could never reproduce a signature. Never rendered by any API read.';

COMMENT ON COLUMN webhook_endpoints.event_filters IS
  'Event types this endpoint receives, exact match; empty means all (public) events. Subscription-target mapping (who receives which project''s events) arrives with T1002 — filters here are the delivery-side half only.';

COMMENT ON COLUMN webhook_endpoints.consecutive_failures IS
  'Consecutive failed deliveries (any status other than delivered). Reset to 0 by a successful delivery; reaching the deliverer''s limit disables the endpoint.';

COMMENT ON COLUMN webhook_endpoints.deleted_at IS
  'Soft delete (docs/04 §6: nothing disappears — state only evolves). DeleteEndpoint stamps it together with enabled=false instead of removing the row: the endpoint answers not-found, receives no fan-out and cannot be redelivered, but its delivery-log rows keep their endpoint_id and stay readable through the list API. NULL = live.';

ALTER TABLE outbox_events
  ADD COLUMN webhook_fanned_out_at timestamptz;

COMMENT ON COLUMN outbox_events.webhook_fanned_out_at IS
  'The webhook fan-out cursor (T1006): set in the same transaction that inserts the delivery rows, so the fan-out is exactly-once per published row — a crash between insert and mark re-runs it as a no-op under webhook_deliveries_endpoint_event_uniq.';

-- The fan-out backlog scan: published rows that no fan-out has consumed.
CREATE INDEX outbox_events_fanout_pending_idx
  ON outbox_events (created_at, id)
  WHERE published_at IS NOT NULL AND webhook_fanned_out_at IS NULL;

ALTER TABLE webhook_deliveries
  ADD COLUMN endpoint_id uuid REFERENCES webhook_endpoints(id) ON DELETE SET NULL,
  ADD COLUMN event_type text NOT NULL DEFAULT '',
  ADD COLUMN created_at timestamptz NOT NULL DEFAULT now(),
  ADD COLUMN next_retry_at timestamptz,
  ADD COLUMN last_error text,
  ADD COLUMN delivered_at timestamptz;

-- A fan-out insert names only (endpoint_id, endpoint, event_id,
-- event_type): every fanned-out delivery starts pending by definition,
-- and the default says so for this and any future writer.
ALTER TABLE webhook_deliveries
  ALTER COLUMN status SET DEFAULT 'pending';

COMMENT ON COLUMN webhook_deliveries.endpoint_id IS
  'The registered endpoint this delivery targets (NULL only for rows predating T1006 — the table exists since 00012 but nothing has ever written it).';

COMMENT ON COLUMN webhook_deliveries.event_type IS
  'The delivered event''s type, pinned at fan-out (the log must read without a join into the append-only event log).';

COMMENT ON COLUMN webhook_deliveries.next_retry_at IS
  'When the next attempt is due (status pending). NULL = due now. Doubles as the in-flight lease: the deliverer claims a row by pushing next_retry_at into the future, so a crash mid-request releases the row after the lease expires instead of blocking it forever.';

COMMENT ON COLUMN webhook_deliveries.last_error IS
  'Why the last attempt failed (bounded text — see the deliverer''s error truncation). The operator''s window onto a stuck delivery, mirroring outbox_events.last_error.';

-- The fan-out idempotency guarantee: one delivery row per (endpoint, event)
-- pair, ever. Retried fan-outs hit ON CONFLICT DO NOTHING and stay no-ops.
CREATE UNIQUE INDEX webhook_deliveries_endpoint_event_uniq
  ON webhook_deliveries (endpoint_id, event_id)
  WHERE endpoint_id IS NOT NULL;

-- The deliverer's due-work scan.
CREATE INDEX webhook_deliveries_due_idx
  ON webhook_deliveries (status, next_retry_at)
  WHERE status = 'pending';

-- +goose Down
DROP INDEX webhook_deliveries_due_idx;
DROP INDEX webhook_deliveries_endpoint_event_uniq;
ALTER TABLE webhook_deliveries
  ALTER COLUMN status DROP DEFAULT,
  DROP COLUMN delivered_at,
  DROP COLUMN last_error,
  DROP COLUMN next_retry_at,
  DROP COLUMN created_at,
  DROP COLUMN event_type,
  DROP COLUMN endpoint_id;
DROP INDEX outbox_events_fanout_pending_idx;
ALTER TABLE outbox_events
  DROP COLUMN webhook_fanned_out_at;
DROP TABLE webhook_endpoints;
