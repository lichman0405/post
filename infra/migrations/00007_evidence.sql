-- +goose Up
-- Evidence assertions (canonical lines 168-183).
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
