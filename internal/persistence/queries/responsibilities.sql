-- Scientific responsibility and Research Owners routing (task T0604,
-- canonical tables research_owner_rules and responsibility_assignments,
-- migration 00084; docs/04 §3).
--
-- These are project configuration rows, not permission rows: nothing here
-- is read by internal/authz, and holding a label buys exactly one thing —
-- the conditional submit_scientific_review verdict resolves, and the
-- review is attributed to the responsibility it was signed under
-- (docs/04 §3「责任用于 Review routing，不自动赋予更高访问权限」).

-- name: ListResearchOwnerRules :many
-- Every routing rule of one project, in a deterministic order (the same
-- order the resolver and the required-review calculation see, so a
-- project's requirements never depend on which index the planner walked).
SELECT * FROM research_owner_rules
WHERE project_id = @project_id
ORDER BY match_kind, match_value, responsibility, id;

-- name: CreateResearchOwnerRule :one
-- The UNIQUE(project_id, match_kind, match_value, responsibility) refuses
-- the same mapping written twice (23505); a different LABEL for the same
-- match is a different row on purpose — that is how one change acquires
-- two responsible reviewers.
INSERT INTO research_owner_rules
    (project_id, match_kind, match_value, responsibility, created_by)
VALUES
    (@project_id, @match_kind, @match_value, @responsibility, @created_by)
RETURNING *;

-- name: DeleteResearchOwnerRule :execrows
-- Project-scoped by construction: a rule id of another project deletes
-- nothing (the caller reports not-found, never touching a foreign row).
DELETE FROM research_owner_rules
WHERE id = @id AND project_id = @project_id;

-- name: ListResponsibilityAssignments :many
-- Who holds which label in the project (the assignment list a project
-- owner reads), in a deterministic order.
SELECT * FROM responsibility_assignments
WHERE project_id = @project_id
ORDER BY responsibility, user_id;

-- name: CreateResponsibilityAssignment :one
-- Holding a label is ONE fact: the primary key (project_id, user_id,
-- responsibility) makes a repeated assignment a no-op rather than a
-- second row, so the resolver's answer cannot depend on how many times
-- the assignment was written. ON CONFLICT DO NOTHING answers no row on a
-- repeat, and the store reads the existing row back (assign is
-- idempotent, never an error).
INSERT INTO responsibility_assignments
    (project_id, user_id, responsibility, created_by)
VALUES
    (@project_id, @user_id, @responsibility, @created_by)
ON CONFLICT (project_id, user_id, responsibility) DO NOTHING
RETURNING *;

-- name: GetResponsibilityAssignment :one
SELECT * FROM responsibility_assignments
WHERE project_id = @project_id AND user_id = @user_id AND responsibility = @responsibility;

-- name: DeleteResponsibilityAssignment :execrows
DELETE FROM responsibility_assignments
WHERE project_id = @project_id AND user_id = @user_id AND responsibility = @responsibility;

-- name: ListResponsibilitiesForUser :many
-- The reviewer-responsibility resolver's read: the labels one user holds
-- in one project, sorted (the resolver returns them in this order, and
-- the review service records the first one that matches a requirement).
SELECT responsibility FROM responsibility_assignments
WHERE project_id = @project_id AND user_id = @user_id
ORDER BY responsibility;
