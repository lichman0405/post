-- +goose Up
-- Research profile content (T0102): profiles is the 1:1 profile projection
-- for a person identity (docs/21 lists users and profiles as separate
-- canonical tables). The identity facts (handle, display_name, email) stay
-- on users; the profile content starts with bio and is where later profile
-- settings (per-field visibility etc.) will land without touching identity.
--
-- bio is a current projection, deliberately NOT append-only: the owner may
-- edit it (docs/21 §4 — the append-only set is the history/ledger tables).
-- The backfill materializes a row for every existing identity so the 1:1
-- shape holds from the moment the migration lands; stores also create the
-- row atomically with the user (credential store CTE, T0102).
CREATE TABLE profiles (
  user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE RESTRICT,
  bio text NOT NULL DEFAULT '',
  updated_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO profiles (user_id) SELECT id FROM users;

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
