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
    (actor_id, via, action, target_ref, project_id, organization_id,
     correlation_id, before_summary, after_summary, metadata)
VALUES
    (@actor_id, @via, @action, @target_ref, @project_id, @organization_id,
     @correlation_id, @before_summary, @after_summary, @metadata)
RETURNING *;

-- name: ListProjectAuditEntries :many
-- Project Activity page: the project's audit rows newest-first, with the
-- actor's handle/display name joined for rendering. Keyset pagination on
-- (occurred_at, id): a nil before pair means "from the top".
SELECT a.id, a.actor_id, a.via, a.action, a.target_ref, a.project_id,
       a.organization_id, a.correlation_id, a.before_summary,
       a.after_summary, a.metadata, a.occurred_at,
       u.handle AS actor_handle, u.display_name AS actor_display_name
FROM audit_log a
LEFT JOIN users u ON u.id = a.actor_id
WHERE a.project_id = @project_id
  AND (@before_ts::timestamptz IS NULL
       OR (a.occurred_at, a.id) < (@before_ts::timestamptz, @before_id::uuid))
ORDER BY a.occurred_at DESC, a.id DESC
LIMIT @page_limit;

-- name: ListOrganizationAuditEntries :many
-- Organization Activity page: the organization's audit rows newest-first,
-- same keyset shape as the project query.
SELECT a.id, a.actor_id, a.via, a.action, a.target_ref, a.project_id,
       a.organization_id, a.correlation_id, a.before_summary,
       a.after_summary, a.metadata, a.occurred_at,
       u.handle AS actor_handle, u.display_name AS actor_display_name
FROM audit_log a
LEFT JOIN users u ON u.id = a.actor_id
WHERE a.organization_id = @organization_id
  AND (@before_ts::timestamptz IS NULL
       OR (a.occurred_at, a.id) < (@before_ts::timestamptz, @before_id::uuid))
ORDER BY a.occurred_at DESC, a.id DESC
LIMIT @page_limit;
