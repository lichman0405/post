-- +goose Up
-- Blobs and their attachments to scientific object versions
-- (canonical lines 185-202).
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
