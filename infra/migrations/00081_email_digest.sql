-- +goose Up
-- Email digest (T1005, docs/18 §4: "Email immediate/digest abstraction";
-- docs/20 §9 lists "notification digest" among the Go worker's jobs). The
-- delivery rows this pipeline consumes already exist — subscription_deliveries
-- rows with channel = 'email' are born 'pending' for exactly this sender
-- (00078) — so this migration adds the two things a *sender* needs that a
-- fan-out does not: the subscriber's cadence preference, and the claim state
-- that makes an at-least-once send safe to retry.
--
--   1. notification_preferences: how often ONE account wants its email
--      notifications — immediate (each event on its own), daily or weekly (the
--      events of the interval in one digest). It is a property of the account,
--      not of a subscription: a subscription already chooses channels
--      (00078's channels column is where "email or not" lives), and asking the
--      same user to restate their cadence once per target would be asking them
--      to keep N copies of one preference in sync.
--
--      A missing row is NOT a special case: it means the default cadence, and
--      the sender reads DefaultCadence (events.NotificationPreferences) rather
--      than treating an absent row as "no email". An account that never
--      opened the setting still has one defined behaviour, and it is the
--      sender — not the presence of a settings row — that decides what that
--      behaviour is.
--
--      last_digest_at is the interval anchor: a digest-style cadence
--      (daily/weekly) is due once this much time has passed since the last
--      one, so the schedule is drift-free (a delayed digest does not push the
--      next one out) and needs no per-user timer table. Immediate does not
--      read it — every event is its own digest — and the column is still
--      stamped for it, so a switch from immediate to daily starts its
--      interval from a real send rather than from the row's created_at.
--
--   2. subscription_deliveries.attempts / leased_until: the claim state of
--      the digest sender. The sender cannot hold a database transaction open
--      across a mail submission (a slow SMTP submission inside a transaction
--      is a lock held for a network round trip, for every subscriber in the
--      batch at once), so it claims under a LEASE instead: the claim stamps
--      leased_until, commits, sends, and marks the row delivered. A row whose
--      send failed is simply due again when the lease expires — the same
--      at-least-once shape the webhook deliverer uses (00059's
--      attempts/next_attempt_at), and the reason a crash mid-send costs a
--      duplicate email rather than a lost one.
--
--      attempts bounds that retry: it is what the sender's withdrawal rule
--      counts, so a delivery that can never be sent is withdrawn with a
--      recorded reason instead of being retried forever. The columns are
--      added to subscription_deliveries rather than to a sender-side table
--      because they are the STATE OF THE DELIVERY ROW the sender is working
--      on: a second table keyed by delivery id would be one more thing that
--      has to agree with the row it describes.
--
--      Both columns are read and written by raw SQL (events.NotificationStore,
--      like every other subscription_deliveries reader) — subscription tables
--      are not modeled by checked-in sqlc queries, so this migration moves no
--      generated code.

CREATE TABLE notification_preferences (
  user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE RESTRICT,
  cadence text NOT NULL DEFAULT 'daily' CHECK (cadence IN ('immediate', 'daily', 'weekly')),
  last_digest_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE notification_preferences IS
  'How often one account receives its email notifications (T1005, docs/18 §4): immediate = one message per event, daily/weekly = one digest per interval. One row per account that has ever changed the setting; NO row means the default cadence (events.DefaultCadence), so absence is never "no email".';

COMMENT ON COLUMN notification_preferences.cadence IS
  'The account''s email cadence (events.CadenceImmediate/CadenceDaily/CadenceWeekly; events.ValidateCadence checks the same vocabulary on the application path). Default daily: docs/18 §3 asks the platform not to notify on every low-level event by default, and email is the channel that leaves the building.';

COMMENT ON COLUMN notification_preferences.last_digest_at IS
  'When this account''s last email digest was actually handed to the mail transport. The interval anchor for a digest cadence: a daily/weekly digest is due once last_digest_at is older than the cadence window (events.DigestDue). NULL = no digest sent yet, which is due immediately — a subscriber never waits a full interval for their first digest.';

ALTER TABLE subscription_deliveries
  ADD COLUMN attempts integer NOT NULL DEFAULT 0,
  ADD COLUMN leased_until timestamptz;

ALTER TABLE subscription_deliveries
  ADD CONSTRAINT subscription_deliveries_attempts_nonnegative CHECK (attempts >= 0);

COMMENT ON COLUMN subscription_deliveries.attempts IS
  'How many times the digest sender has claimed this row (T1005). Counted against the sender''s withdrawal limit: a row that exhausts it is withdrawn with a recorded reason rather than retried forever. Web rows never increment it — the inbox IS their delivery.';

COMMENT ON COLUMN subscription_deliveries.leased_until IS
  'The claim lease the digest sender holds on this row: a pending row whose lease is set and not yet expired is being sent by a worker and is not claimable again. NULL (or expired) = claimable. The lease is what lets the send happen outside a transaction without two senders mailing the same notification.';

-- The digest sender's claim scan: pending email rows, oldest first, whether
-- or not they are currently leased (the claim filters on the lease itself —
-- the index only has to make the partial predicate cheap).
CREATE INDEX subscription_deliveries_email_pending_idx
  ON subscription_deliveries (created_at, id)
  WHERE channel = 'email' AND status = 'pending';

-- +goose Down
DROP INDEX subscription_deliveries_email_pending_idx;
ALTER TABLE subscription_deliveries
  DROP CONSTRAINT subscription_deliveries_attempts_nonnegative,
  DROP COLUMN leased_until,
  DROP COLUMN attempts;
DROP TABLE notification_preferences;
