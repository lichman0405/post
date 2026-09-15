-- Research PR merge governance (canonical table: merge_creations). The
-- Idempotency-Key ledger of the merge endpoint (docs/22 §3): the same key
-- replays the merge it created, forever.

-- name: GetMergeCreation :one
SELECT merge_id FROM merge_creations
WHERE project_id = @project_id AND idempotency_key = @idempotency_key;

-- name: CreateMergeCreation :one
INSERT INTO merge_creations (project_id, idempotency_key, merge_id)
VALUES (@project_id, @idempotency_key, @merge_id)
RETURNING *;
