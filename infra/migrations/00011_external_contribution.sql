-- +goose Up
-- External references, contribution ledger events, credit disputes
-- (canonical lines 314-354).
CREATE TABLE external_references (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  source_type text NOT NULL,
  external_identifier text NOT NULL,
  canonical_url text,
  UNIQUE(source_type,external_identifier)
);
CREATE TABLE external_reference_snapshots (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  external_reference_id uuid NOT NULL REFERENCES external_references(id) ON DELETE RESTRICT,
  accessed_at timestamptz NOT NULL,
  upstream_version text,
  metadata jsonb NOT NULL,
  snapshot_hash text NOT NULL,
  blob_id uuid REFERENCES blobs(id) ON DELETE RESTRICT
);

CREATE TABLE contribution_events (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  actor_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  organization_id_at_time uuid REFERENCES organizations(id) ON DELETE RESTRICT,
  project_id uuid REFERENCES projects(id) ON DELETE RESTRICT,
  event_type text NOT NULL,
  role_codes text[] NOT NULL DEFAULT '{}',
  object_refs jsonb NOT NULL DEFAULT '[]'::jsonb,
  accepted_context boolean NOT NULL DEFAULT false,
  released_context boolean NOT NULL DEFAULT false,
  occurred_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE credit_disputes (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid REFERENCES projects(id) ON DELETE RESTRICT,
  opened_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  target_ref text NOT NULL,
  claim text NOT NULL,
  state text NOT NULL DEFAULT 'open' CHECK(state IN ('open','resolved','rejected')),
  resolution text,
  opened_at timestamptz NOT NULL DEFAULT now(),
  resolved_at timestamptz
);
