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

-- name: ListProjectActivity :many
-- Project Activity page (T0110, extended by T0607): the project's
-- governance rows (audit_log) and its research events (research_events),
-- newest first, as ONE key-set-paginated sequence on (occurred_at, id),
-- with the actor's handle/display name joined for rendering. A nil before
-- pair means "from the top".
--
-- Why a union rather than two reads the caller merges: a page is one
-- window, and the window has to be cut by the database. Two independent
-- LIMITs produce two pages whose boundaries do not line up — the merged
-- result would either duplicate rows across pages or lose them, depending
-- on how the caller weaves them — so the ordering and the cursor test
-- happen where the rows are: here.
--
-- The two branches are the two registries docs/26 §1 keeps apart, and the
-- source column is which one a row came from, not a derived label: the
-- audit branch is literally the audit_log read and the research branch is
-- literally the research_events read. Both branches carry the same keyset
-- predicate written twice on purpose — it is what lets each branch ride
-- its own (project_id, occurred_at DESC, id DESC) index instead of
-- scanning the project's whole history to apply the cursor afterwards
-- (00107 adds the research_events index the second branch needs).
--
-- include_governance/include_research are the source filter the reader
-- sent ("" = both): a false branch reads nothing, which is how a filtered
-- page stays one window rather than becoming a second query shape. The
-- filter is applied inside each branch beside the keyset predicate so the
-- plan of a filtered page is the plan of the unfiltered one minus a
-- branch.
--
-- The two branches do NOT share an audience rule, and that asymmetry is
-- deliberate rather than an oversight waiting to be tidied away.
--
-- The GOVERNANCE branch is the audit_log read. That table carries no
-- per-row visibility column at all (00012's column list), so "a project's
-- activity is as visible as the project" is the only rule available to it,
-- and the project read gate its caller already ran has applied that rule.
-- The reader input below cannot narrow this branch, because there is no
-- column here to narrow it by; every audit row comes back whatever its
-- action.
--
-- The RESEARCH branch is the research_events read, and those rows DO carry
-- a visibility column whose meaning the write path fixes (rsg/events.go:
-- an event is never more visible than its subject, so a write committed to
-- a private branch is stored 'private'). A PUBLIC project's read is
-- allowed for every matrix class (read_public_project), so "the gate
-- passed" does not mean "the caller is a member" — the gate is not enough
-- for these rows. The reader is therefore an explicit input of this read
-- (@reader_user_id; ADR-024's third outlet), and a research row is
-- rendered to that reader when EITHER
--
--   e.visibility = 'public'   the row's own axis: a thing nothing
--                             explicitly made public is not rendered
--                             outside its project (00012, docs/12 §5); or
--   the reader holds a project_memberships row for THIS project (e.project_id)
--                             — the same criterion projects.
--                             ProjectStore.GetMembership answers, expressed
--                             here so the filter is the read's rather than
--                             its caller's.
--
-- A future edit must NOT "tidy" the two axes into one
-- `visibility = 'public'` predicate: a member must still see her own
-- project's private rows, and a predicate written for the anonymous
-- audience would take exactly those rows away from her. It is the same
-- discipline research_profile.sql:36-38 records for the anonymous reads and
-- ADR-024's 决定 2-3 records for this one.
--
-- Fail closed: a reader that resolves to no user id arrives as SQL NULL,
-- and the membership clause is then NULL rather than true, so an
-- unresolvable reader gets exactly the public rows — the same direction as
-- the column's own default and rsg/events.go's "anything unclear stays
-- private".
--
-- Organization scope, target ref and the before/after summaries are NULL
-- on every research row by construction — research events have no such
-- columns (00012) — and the payload/visibility columns are NULL on every
-- audit row. The row's source says which half is live, so a client never
-- has to infer it from which fields are NULL.
SELECT activity.*, u.handle AS actor_handle, u.display_name AS actor_display_name
FROM (
    SELECT 'governance'::text AS source,
           a.id, a.actor_id, a.via, a.action, a.target_ref, a.project_id,
           a.organization_id, a.correlation_id, a.before_summary,
           a.after_summary, a.metadata,
           NULL::jsonb AS payload,
           NULL::text AS visibility,
           a.occurred_at
    FROM audit_log a
    WHERE a.project_id = @project_id
      AND @include_governance::bool
      AND (@before_ts::timestamptz IS NULL
           OR (a.occurred_at, a.id) < (@before_ts::timestamptz, @before_id::uuid))
    UNION ALL
    SELECT 'research'::text AS source,
           e.id, e.actor_id, COALESCE(e.via, '')::text AS via, e.event_type,
           NULL::text AS target_ref, e.project_id, NULL::uuid AS organization_id,
           e.correlation_id, NULL::jsonb AS before_summary,
           NULL::jsonb AS after_summary, NULL::jsonb AS metadata,
           e.payload, e.visibility, e.occurred_at
    FROM research_events e
    WHERE e.project_id = @project_id
      AND @include_research::bool
      AND (e.visibility = 'public'
           OR EXISTS (SELECT 1 FROM project_memberships pm
                      WHERE pm.project_id = e.project_id
                        AND pm.user_id = @reader_user_id))
      AND (@before_ts::timestamptz IS NULL
           OR (e.occurred_at, e.id) < (@before_ts::timestamptz, @before_id::uuid))
) activity
LEFT JOIN users u ON u.id = activity.actor_id
ORDER BY activity.occurred_at DESC, activity.id DESC
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
