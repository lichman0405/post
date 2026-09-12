-- POST V1 canonical PostgreSQL schema seed.
-- Claude Code should convert this into the chosen migration/ORM format while preserving constraints.

CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE users (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  handle text UNIQUE NOT NULL,
  email text UNIQUE,
  display_name text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  disabled_at timestamptz
);

CREATE TABLE organizations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  slug text UNIQUE NOT NULL,
  name text NOT NULL,
  description text,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE organization_memberships (
  organization_id uuid NOT NULL REFERENCES organizations(id) ON DELETE RESTRICT,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  role text NOT NULL CHECK (role IN ('owner','maintainer','contributor','viewer')),
  affiliation_start date,
  affiliation_end date,
  verified boolean NOT NULL DEFAULT false,
  PRIMARY KEY (organization_id,user_id)
);

CREATE TABLE programs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid REFERENCES organizations(id) ON DELETE RESTRICT,
  slug text NOT NULL,
  name text NOT NULL,
  description text,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (organization_id, slug)
);

CREATE TABLE projects (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid REFERENCES organizations(id) ON DELETE RESTRICT,
  program_id uuid REFERENCES programs(id) ON DELETE SET NULL,
  slug text NOT NULL,
  name text NOT NULL,
  purpose text NOT NULL,
  activity_status text NOT NULL DEFAULT 'planning' CHECK (activity_status IN ('planning','active','paused','archived')),
  visibility text NOT NULL CHECK (visibility IN ('public','private')),
  main_frozen boolean NOT NULL DEFAULT false,
  git_repository_external_id text,
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (organization_id,slug)
);

CREATE TABLE project_memberships (
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  role text NOT NULL CHECK (role IN ('owner','maintainer','contributor','viewer')),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(project_id,user_id)
);

CREATE TABLE policy_versions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid REFERENCES organizations(id) ON DELETE RESTRICT,
  project_id uuid REFERENCES projects(id) ON DELETE RESTRICT,
  version text NOT NULL,
  policy_json jsonb NOT NULL,
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK ((organization_id IS NOT NULL) <> (project_id IS NOT NULL))
);

CREATE TABLE branches (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  name text NOT NULL,
  visibility text NOT NULL CHECK (visibility IN ('public','private')),
  purpose text,
  git_ref text NOT NULL,
  base_state_id uuid,
  lifecycle_state text NOT NULL DEFAULT 'active' CHECK (lifecycle_state IN ('active','merged','aborted')),
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(project_id,name)
);

CREATE TABLE project_states (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  branch_id uuid REFERENCES branches(id) ON DELETE RESTRICT,
  parent_state_id uuid REFERENCES project_states(id) ON DELETE RESTRICT,
  state_hash text NOT NULL,
  git_commit_sha text,
  manifest_version text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(project_id,state_hash)
);
ALTER TABLE branches ADD CONSTRAINT branches_base_state_fk FOREIGN KEY(base_state_id) REFERENCES project_states(id) ON DELETE RESTRICT;

CREATE TABLE state_commits (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  branch_id uuid NOT NULL REFERENCES branches(id) ON DELETE RESTRICT,
  base_state_id uuid REFERENCES project_states(id) ON DELETE RESTRICT,
  result_state_id uuid NOT NULL REFERENCES project_states(id) ON DELETE RESTRICT,
  actor_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  via text NOT NULL CHECK (via IN ('web','api','mcp','claude_code','git_compat','system')),
  message text NOT NULL,
  operation_summary jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE scientific_objects (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  object_type text NOT NULL,
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE scientific_object_versions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  object_id uuid NOT NULL REFERENCES scientific_objects(id) ON DELETE RESTRICT,
  version_no integer NOT NULL CHECK(version_no > 0),
  state_id uuid NOT NULL REFERENCES project_states(id) ON DELETE RESTRICT,
  branch_id uuid REFERENCES branches(id) ON DELETE RESTRICT,
  schema_id text NOT NULL,
  schema_version text NOT NULL,
  title text NOT NULL,
  lifecycle_state text NOT NULL CHECK (lifecycle_state IN ('active','aborted','reopened','superseded')),
  payload jsonb NOT NULL,
  visibility_policy_id uuid,
  integrity_hash text NOT NULL,
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(object_id,version_no)
);
CREATE INDEX scientific_object_versions_payload_gin ON scientific_object_versions USING gin(payload);

CREATE TABLE relations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE relation_versions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  relation_id uuid NOT NULL REFERENCES relations(id) ON DELETE RESTRICT,
  version_no integer NOT NULL CHECK(version_no > 0),
  state_id uuid NOT NULL REFERENCES project_states(id) ON DELETE RESTRICT,
  relation_type text NOT NULL,
  source_object_version_id uuid NOT NULL REFERENCES scientific_object_versions(id) ON DELETE RESTRICT,
  target_object_version_id uuid NOT NULL REFERENCES scientific_object_versions(id) ON DELETE RESTRICT,
  payload jsonb NOT NULL DEFAULT '{}'::jsonb,
  integrity_hash text NOT NULL,
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(relation_id,version_no)
);
CREATE INDEX relation_versions_source_idx ON relation_versions(source_object_version_id, relation_type);
CREATE INDEX relation_versions_target_idx ON relation_versions(target_object_version_id, relation_type);

CREATE TABLE evidence_assertions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  state_id uuid NOT NULL REFERENCES project_states(id) ON DELETE RESTRICT,
  target_object_version_id uuid NOT NULL REFERENCES scientific_object_versions(id) ON DELETE RESTRICT,
  evidence_object_version_id uuid NOT NULL REFERENCES scientific_object_versions(id) ON DELETE RESTRICT,
  relation_type text NOT NULL CHECK (relation_type IN ('supports','contradicts','consistent_with','inconsistent_with','reproduces','fails_to_reproduce','validates','challenges','contextualizes')),
  evidence_type text NOT NULL,
  scope jsonb NOT NULL DEFAULT '{}'::jsonb,
  directness text NOT NULL DEFAULT 'unknown',
  inference_nature text NOT NULL DEFAULT 'unknown',
  reasoning_note text,
  review_state text NOT NULL DEFAULT 'unreviewed' CHECK(review_state IN ('unreviewed','reviewed','rejected')),
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE blobs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  content_hash text NOT NULL,
  size_bytes bigint NOT NULL CHECK(size_bytes >= 0),
  media_type text,
  storage_key text NOT NULL,
  integrity_state text NOT NULL DEFAULT 'pending' CHECK(integrity_state IN ('pending','verified','failed')),
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(content_hash,size_bytes)
);
CREATE TABLE blob_attachments (
  blob_id uuid NOT NULL REFERENCES blobs(id) ON DELETE RESTRICT,
  scientific_object_version_id uuid NOT NULL REFERENCES scientific_object_versions(id) ON DELETE RESTRICT,
  attachment_role text NOT NULL,
  access_level text NOT NULL CHECK(access_level IN ('open','restricted')),
  PRIMARY KEY(blob_id, scientific_object_version_id, attachment_role)
);

CREATE TABLE issues (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  number bigint NOT NULL,
  issue_type text NOT NULL,
  title text NOT NULL,
  body text NOT NULL DEFAULT '',
  state text NOT NULL DEFAULT 'open' CHECK(state IN ('open','in_progress','closed','aborted')),
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(project_id,number)
);

CREATE TABLE pull_requests (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  number bigint NOT NULL,
  source_branch_id uuid NOT NULL REFERENCES branches(id) ON DELETE RESTRICT,
  target_branch_id uuid NOT NULL REFERENCES branches(id) ON DELETE RESTRICT,
  base_state_id uuid NOT NULL REFERENCES project_states(id) ON DELETE RESTRICT,
  proposed_state_id uuid NOT NULL REFERENCES project_states(id) ON DELETE RESTRICT,
  title text NOT NULL,
  body text NOT NULL DEFAULT '',
  state text NOT NULL DEFAULT 'open',
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  merged_at timestamptz,
  UNIQUE(project_id,number)
);

CREATE TABLE reviews (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  pull_request_id uuid NOT NULL REFERENCES pull_requests(id) ON DELETE RESTRICT,
  reviewer_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  review_kind text NOT NULL CHECK(review_kind IN ('scientific','integrity','rights','ip')),
  decision text NOT NULL CHECK(decision IN ('approved','changes_requested','commented')),
  body text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE validation_results (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  state_id uuid NOT NULL REFERENCES project_states(id) ON DELETE RESTRICT,
  gate text NOT NULL CHECK(gate IN ('pr','main','release','asset')),
  status text NOT NULL CHECK(status IN ('passed','failed','warning')),
  result_json jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE releases (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  version text NOT NULL,
  title text NOT NULL,
  state_id uuid NOT NULL REFERENCES project_states(id) ON DELETE RESTRICT,
  policy_version_id uuid REFERENCES policy_versions(id) ON DELETE RESTRICT,
  manifest jsonb NOT NULL,
  manifest_hash text NOT NULL,
  created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(project_id,version)
);

CREATE TABLE research_assets (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  asset_type text NOT NULL CHECK(asset_type IN ('dataset','protocol','material_collection','benchmark')),
  slug text NOT NULL,
  title text NOT NULL,
  origin_project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE research_asset_versions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  asset_id uuid NOT NULL REFERENCES research_assets(id) ON DELETE RESTRICT,
  version text NOT NULL,
  source_release_id uuid REFERENCES releases(id) ON DELETE RESTRICT,
  manifest jsonb NOT NULL,
  rights_json jsonb NOT NULL,
  visibility text NOT NULL CHECK(visibility IN ('public','private')),
  integrity_hash text NOT NULL,
  published_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  published_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(asset_id,version)
);
CREATE TABLE asset_lineage (
  parent_asset_version_id uuid NOT NULL REFERENCES research_asset_versions(id) ON DELETE RESTRICT,
  child_asset_version_id uuid NOT NULL REFERENCES research_asset_versions(id) ON DELETE RESTRICT,
  relation_type text NOT NULL CHECK(relation_type IN ('forked_from','derived_from','supersedes')),
  PRIMARY KEY(parent_asset_version_id,child_asset_version_id,relation_type)
);
CREATE TABLE asset_dependencies (
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  asset_version_id uuid NOT NULL REFERENCES research_asset_versions(id) ON DELETE RESTRICT,
  dependency_type text NOT NULL,
  visibility_of_usage text NOT NULL DEFAULT 'private' CHECK(visibility_of_usage IN ('public','private')),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(project_id,asset_version_id,dependency_type)
);

CREATE TABLE knowledge_publications (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  object_version_id uuid NOT NULL REFERENCES scientific_object_versions(id) ON DELETE RESTRICT,
  public_version text NOT NULL,
  rights_json jsonb NOT NULL,
  published_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  published_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(object_version_id,public_version)
);

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

-- Search projection (rebuildable)
CREATE TABLE search_documents (
  entity_ref text PRIMARY KEY,
  entity_type text NOT NULL,
  visibility text NOT NULL,
  project_id uuid REFERENCES projects(id) ON DELETE RESTRICT,
  title text NOT NULL,
  content text NOT NULL,
  structured jsonb NOT NULL DEFAULT '{}'::jsonb,
  embedding vector(1536),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX search_documents_fts_idx ON search_documents USING gin(to_tsvector('simple', title || ' ' || content));
CREATE INDEX search_documents_structured_gin ON search_documents USING gin(structured);
