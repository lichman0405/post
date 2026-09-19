-- +goose Up
-- T0901. Two pieces of the search projection land here: the consumer's
-- cursor, and the target shape a knowledge subscription is addressed by.
--
--   1. search_projected_events: the search projection's exactly-once
--      cursor, one row per outbox row the projector has consumed. It is
--      the third consumer to keep a cursor of its own — the webhook
--      fan-out keeps one as a column on outbox_events
--      (00059's webhook_fanned_out_at), the subscription fan-out keeps one
--      as this table's twin (00078's subscription_fanned_events) — and it
--      is a table rather than a column on outbox_events for the same
--      reason 00078 chose a table: outbox_events is modeled by checked-in
--      sqlc queries (SELECT * / INSERT ... RETURNING *), so a new column
--      there moves generated code. The semantics are the other two's,
--      exactly: the cursor row is written in the same transaction as the
--      projected document, so a crash between the two re-runs an idempotent
--      upsert instead of losing an event, and no consumer's progress is
--      visible to another's.
--
--   2. subscriptions_target_id_shape: a knowledge target becomes
--      PID-addressed, exactly as an asset target already is. 00078's
--      CHECK gave the asset pid its own branch because the asset page
--      addresses an asset by its pid and the pid never changes; the
--      knowledge publication has the same identity shape (00083's
--      knowledge_publications_pid_format carries the identical regexp),
--      but its row uuid never leaves the process — the public read
--      resolves a publication BY PID (cmd/api/knowledgehttp: "Read
--      resolves one published knowledge object by its pid") — so the uuid
--      a knowledge target used to demand was an identifier no user could
--      ever hold, and a knowledge subscription could not be created at all.
--      events.ValidateTargetID carries the same rule on the application
--      path; this CHECK is the second line, for a session that writes SQL
--      directly.

CREATE TABLE search_projected_events (
  outbox_event_id uuid PRIMARY KEY REFERENCES outbox_events(id) ON DELETE RESTRICT,
  projected_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE search_projected_events IS
  'The search projection''s exactly-once cursor (T0901): one row per outbox row the projector has consumed, written in the same transaction as the search_documents upsert, so a crash between the two re-runs an idempotent upsert instead of losing an event. It mirrors subscription_fanned_events (00078) and webhook_fanned_out_at (00059): every consumer of the outbox keeps its own progress.';

-- The knowledge branch of the target-id shape moves from the uuid form to
-- the 26-character Crockford base32 PID — the same regexp the asset branch
-- has carried since 00078, and the one knowledge_publications_pid_format
-- (00083) enforces on the pid column itself.
--
-- The data step is not optional: a knowledge target_id written under the
-- old rule is the publication's ROW UUID, and that uuid fails the new
-- CHECK. Every such row names a real publication — the subscribe path
-- resolves the target's audience through TargetAudience before it writes,
-- and a publication row is referenced by knowledge_publication_creations
-- (ON DELETE RESTRICT, 00083) so it cannot be deleted — which is why the
-- conversion below is total: it rewrites every existing row's target_id to
-- the pid of the publication the old uuid named, and the constraint that
-- follows accepts every row. A row whose uuid named no publication cannot
-- exist (the subscribe would have been refused with AudienceNone), so the
-- ALTER ADD CONSTRAINT below is not a step that can fail on live data.
ALTER TABLE subscriptions
  DROP CONSTRAINT subscriptions_target_id_shape;

UPDATE subscriptions s
SET target_id = kp.pid
FROM knowledge_publications kp
WHERE s.target_type = 'knowledge' AND s.target_id = kp.id::text;

ALTER TABLE subscriptions
  ADD CONSTRAINT subscriptions_target_id_shape
    CHECK (CASE target_type
             WHEN 'asset' THEN target_id ~ '^[0-9a-hjkmnp-tv-z]{26}$'
             WHEN 'knowledge' THEN target_id ~ '^[0-9a-hjkmnp-tv-z]{26}$'
             ELSE target_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
           END);

COMMENT ON TABLE subscriptions IS
  'One user following one target (T1002, docs/18 §3). target_type is the canonical target vocabulary; target_id is the target''s stable identity — a uuid for project/user/organization, the asset pid for asset (the same identifier /assets/{pid} is built from), the publication pid for knowledge (the same identifier /api/v1/knowledge/{pid} is built from, 00083). deleted_at is the unsubscribe marker: a deleted subscription matches no fan-out, and its in-flight deliveries are cancelled in the same transaction.';

-- +goose Down
ALTER TABLE subscriptions
  DROP CONSTRAINT subscriptions_target_id_shape;

UPDATE subscriptions s
SET target_id = kp.id::text
FROM knowledge_publications kp
WHERE s.target_type = 'knowledge' AND s.target_id = kp.pid;

ALTER TABLE subscriptions
  ADD CONSTRAINT subscriptions_target_id_shape
    CHECK (CASE target_type
             WHEN 'asset' THEN target_id ~ '^[0-9a-hjkmnp-tv-z]{26}$'
             ELSE target_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
           END);

COMMENT ON TABLE subscriptions IS
  'One user following one target (T1002, docs/18 §3). target_type is the canonical target vocabulary; target_id is the target''s stable identity — a uuid for project/knowledge/user/organization, the asset pid for asset (the same identifier /assets/{pid} is built from). deleted_at is the unsubscribe marker: a deleted subscription matches no fan-out, and its in-flight deliveries are cancelled in the same transaction.';

DROP TABLE search_projected_events;
