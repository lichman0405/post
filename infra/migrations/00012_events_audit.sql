-- +goose Up
-- Domain events, transactional outbox, subscriptions, webhook deliveries,
-- audit log (canonical lines 356-408).
CREATE TABLE research_events (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  event_type text NOT NULL,
  actor_id uuid REFERENCES users(id) ON DELETE RESTRICT,
  project_id uuid REFERENCES projects(id) ON DELETE RESTRICT,
  visibility text NOT NULL,
  payload jsonb NOT NULL,
  correlation_id text NOT NULL,
  occurred_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE outbox_events (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  event_type text NOT NULL,
  payload jsonb NOT NULL,
  correlation_id text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  published_at timestamptz,
  attempts integer NOT NULL DEFAULT 0
);

CREATE TABLE subscriptions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  target_type text NOT NULL,
  target_id text NOT NULL,
  event_filters text[] NOT NULL DEFAULT '{}',
  channels text[] NOT NULL DEFAULT '{web}',
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE webhook_deliveries (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  event_id uuid NOT NULL REFERENCES research_events(id) ON DELETE RESTRICT,
  endpoint text NOT NULL,
  status text NOT NULL,
  response_code integer,
  attempts integer NOT NULL DEFAULT 0,
  last_attempt_at timestamptz
);

CREATE TABLE audit_log (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  actor_id uuid REFERENCES users(id) ON DELETE RESTRICT,
  via text NOT NULL,
  action text NOT NULL,
  target_ref text,
  project_id uuid REFERENCES projects(id) ON DELETE RESTRICT,
  correlation_id text NOT NULL,
  before_summary jsonb,
  after_summary jsonb,
  metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
  occurred_at timestamptz NOT NULL DEFAULT now()
);
