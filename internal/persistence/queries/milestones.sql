-- Project milestones (T0609): research-timeline markers, separate from
-- releases (canonical tables project_milestones,
-- project_milestone_creations, migration 00063). A milestone may name a
-- release; it never requires one.

-- name: CreateMilestone :one
INSERT INTO project_milestones (project_id, kind, label, occurred_at, release_id, created_by)
VALUES (@project_id, @kind, @label, @occurred_at, @release_id, @created_by)
RETURNING *;

-- name: ListMilestones :many
-- The timeline: one project's milestones in research order — occurred_at
-- first (the event's date, the canonical kinds' natural progression),
-- then creation order (created_at, id) so same-date ties are a total,
-- deterministic order no matter which order the rows were inserted.
SELECT * FROM project_milestones
WHERE project_id = @project_id
ORDER BY occurred_at, created_at, id;

-- name: GetMilestone :one
SELECT * FROM project_milestones
WHERE project_id = @project_id AND id = @id;

-- name: GetMilestoneCreation :one
SELECT milestone_id FROM project_milestone_creations
WHERE project_id = @project_id AND idempotency_key = @idempotency_key;

-- name: CreateMilestoneCreation :one
INSERT INTO project_milestone_creations (project_id, idempotency_key, milestone_id)
VALUES (@project_id, @idempotency_key, @milestone_id)
RETURNING *;
