-- +goose Up
-- Identity: users, organizations, memberships (canonical lines 7-32).
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
