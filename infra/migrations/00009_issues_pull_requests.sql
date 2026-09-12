-- +goose Up
-- Issues, pull requests, reviews (canonical lines 204-242).
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
