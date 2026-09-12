-- Domain events, transactional outbox, audit log (canonical tables:
-- research_events, outbox_events, audit_log). Outbox rows are written in the
-- same transaction as the state change (docs/53).

-- name: RecordResearchEvent :one
INSERT INTO research_events (event_type, actor_id, project_id, visibility, payload, correlation_id)
VALUES (@event_type, @actor_id, @project_id, @visibility, @payload, @correlation_id)
RETURNING *;

-- name: EnqueueOutboxEvent :one
INSERT INTO outbox_events (event_type, payload, correlation_id)
VALUES (@event_type, @payload, @correlation_id)
RETURNING *;

-- name: ListPendingOutboxEvents :many
SELECT * FROM outbox_events
WHERE published_at IS NULL
ORDER BY created_at, id
LIMIT @batch_size;

-- name: MarkOutboxEventPublished :exec
UPDATE outbox_events
SET published_at = now(), attempts = attempts + 1
WHERE id = @id;

-- name: RecordAuditLogEntry :one
INSERT INTO audit_log
    (actor_id, via, action, target_ref, project_id, correlation_id,
     before_summary, after_summary, metadata)
VALUES
    (@actor_id, @via, @action, @target_ref, @project_id, @correlation_id,
     @before_summary, @after_summary, @metadata)
RETURNING *;
