-- +goose Up
-- The research inbox (T1003): the read/unread half of the notification
-- row, on top of the delivery rows T1002's subscription fan-out already
-- writes.
--
-- T1002 left the delivery row as a POINTER (subscription, event, channel,
-- target, event type — never event content) and made the web channel born
-- 'delivered', with this note in its own migration: "the web channel is
-- delivered the moment the row exists — the inbox row IS the delivery, and
-- read/unread on top of it is T1003". This migration adds exactly that
-- marker. Nothing else about the delivery changes: the row is still one
-- notification, still owner-scoped, still a pointer.
--
-- Why a column on the delivery rather than a second table (for example a
-- per-user "inbox entries" table that the fan-out would upsert):
--
--   * the delivery row is already the unit a fan-out pass can produce at
--     most once (subscription_deliveries_uniq), so read state lands on a
--     row that cannot be duplicated — no second write path, no second
--     cursor, no window in which a listed notification has no read state;
--   * the AGGREGATION the inbox renders ("12 commits on this project in
--     this hour") is a GROUP BY over these rows, not a stored object: a
--     stored entry would have to be maintained when a delivery is
--     cancelled by the revocation path (T1002's cancelUndeliverable-
--     Deliveries), and every such maintenance is a chance for the inbox
--     to show a notification the authorization rule already withdrew.
--     Derived from live rows, a withdrawn delivery simply stops counting;
--   * read state is per delivery, so "mark this hour's burst read" is an
--     UPDATE over the rows that were actually shown, and a delivery that
--     arrives a second later is NOT silently marked read by it.
--
-- read_at IS NULL means unread. There is no boolean: when it was read is
-- worth keeping (an unread count is a question about the present, the
-- read time is a fact about the past), and NULL is the only value that
-- cannot be confused with a real timestamp.
--
-- The CHECK pairs read_at with 'delivered' the way 00078 pairs
-- delivered_at/cancelled_at with their statuses: a 'pending' row is one
-- the email digest has not sent yet and a 'cancelled' row is one the
-- authorization rule withdrew, so neither has been SHOWN to anyone and
-- neither can be read. The pairing is what keeps "unread" from being
-- satisfiable by rows that were never in the inbox to begin with.
ALTER TABLE subscription_deliveries
  ADD COLUMN read_at timestamptz;

ALTER TABLE subscription_deliveries
  ADD CONSTRAINT subscription_deliveries_read_at_delivered
    CHECK (read_at IS NULL OR status = 'delivered');

COMMENT ON COLUMN subscription_deliveries.read_at IS
  'When the subscriber read this notification; NULL = unread. Set only on delivered rows (the CHECK pairs it with status), and only by the inbox''s own mark-read path (T1003). The inbox aggregates these rows by (target, event type, hour) instead of storing an entry object, so a delivery the T1002 revocation path cancels stops counting toward an entry rather than leaving a stale one behind.';

-- The mass "mark my inbox read" update: one subscriber's unread delivered
-- web rows. The predicate and the leading column are the WHERE clause of
-- markInboxAllRead (internal/events/inbox_store.go) — user_id,
-- channel = 'web', status = 'delivered', read_at IS NULL — matched
-- element for element, so the statement has an index of its own shape to
-- find its rows through.
--
-- It does NOT answer the badge or the "unread" view, and no index of that
-- shape can: the entries read needs the READ rows too (an entry's count is
-- the same in both views, and the "all" view lists read entries), so a
-- partial index whose predicate excludes them can never serve it.
-- subscription_deliveries_inbox_idx (00078) is the index for that read.
CREATE INDEX subscription_deliveries_unread_idx
  ON subscription_deliveries (user_id, created_at DESC, id DESC)
  WHERE channel = 'web' AND status = 'delivered' AND read_at IS NULL;

-- +goose Down
DROP INDEX subscription_deliveries_unread_idx;
ALTER TABLE subscription_deliveries
  DROP CONSTRAINT subscription_deliveries_read_at_delivered,
  DROP COLUMN read_at;
