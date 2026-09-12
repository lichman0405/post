-- Blobs and their attachments (canonical tables: blobs, blob_attachments).
-- Blob rows are metadata only; bytes live in S3/MinIO (invariant 7: knowledge
-- visibility != blob accessibility).

-- name: CreateBlob :one
INSERT INTO blobs (content_hash, size_bytes, media_type, storage_key, created_by)
VALUES (@content_hash, @size_bytes, @media_type, @storage_key, @created_by)
RETURNING *;

-- name: GetBlobByContentHash :one
SELECT * FROM blobs
WHERE content_hash = @content_hash AND size_bytes = @size_bytes;

-- name: MarkBlobIntegrity :exec
UPDATE blobs SET integrity_state = @integrity_state
WHERE id = @id;

-- name: AttachBlob :exec
INSERT INTO blob_attachments (blob_id, scientific_object_version_id, attachment_role, access_level)
VALUES (@blob_id, @scientific_object_version_id, @attachment_role, @access_level);
