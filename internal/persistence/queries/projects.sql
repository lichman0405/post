-- Projects, programs, memberships (canonical tables: programs, projects,
-- project_memberships).

-- name: CreateProgram :one
INSERT INTO programs (organization_id, slug, name, description)
VALUES (@organization_id, @slug, @name, @description)
RETURNING *;

-- name: CreateProject :one
INSERT INTO projects (organization_id, program_id, slug, name, purpose, visibility, created_by)
VALUES (@organization_id, @program_id, @slug, @name, @purpose, @visibility, @created_by)
RETURNING *;

-- name: GetProjectByID :one
SELECT * FROM projects WHERE id = @id;

-- name: GetProjectByIDForUpdate :one
-- The project-scoped lock serializing membership writes: role changes run
-- inside a transaction and lock the project row first, so the last-owner
-- count can never race a concurrent demotion.
SELECT * FROM projects WHERE id = @id FOR UPDATE;

-- name: GetProjectBySlug :one
SELECT * FROM projects WHERE organization_id = @organization_id AND slug = @slug;

-- FreezeProjectMain is the freeze governance action's write (T0601). It is
-- a compare-and-swap, not a read-then-write: the flag moves from false to
-- true only while it is still false, so of two concurrent freezes of one
-- project exactly ONE finds a row here and the loser finds zero — and only
-- the winner's audit row and domain event are written (the task's
-- concurrency criterion; the same CAS discipline
-- scientific_objects.current_version_no follows).
--
-- Zero rows mean either the project does not exist or main is already
-- frozen; the adapter distinguishes them with one read, exactly the way
-- UpdateBranchBaseState's zero-row outcome is resolved (state_store.go).
--
-- There is deliberately no statement in this file that CLEARS the flag:
-- V1 provides no unfreeze (docs/09 §3: "Emergency unfreeze 不在 V1 提供，
-- 避免形成绕过路径"; specs/api/openapi.yaml carries no :unfreeze). Adding
-- one here would be the bypass the specification forbids.
-- name: FreezeProjectMain :one
UPDATE projects
SET main_frozen = true
WHERE id = @id AND main_frozen = false
RETURNING *;

-- name: ListProjectsByOrganization :many
SELECT * FROM projects
WHERE organization_id = @organization_id
ORDER BY created_at, id
LIMIT @page_size OFFSET @page_offset;

-- name: UpdateProjectActivityStatus :one
UPDATE projects
SET activity_status = @activity_status
WHERE id = @id
RETURNING *;

-- name: UpdateProjectSettings :one
UPDATE projects
SET purpose = @purpose,
    activity_status = @activity_status
WHERE id = @id
RETURNING *;

-- name: AddProjectMembership :one
INSERT INTO project_memberships (project_id, user_id, role)
VALUES (@project_id, @user_id, @role)
RETURNING *;

-- name: UpdateProjectMembershipRole :one
UPDATE project_memberships
SET role = @role
WHERE project_id = @project_id AND user_id = @user_id
RETURNING *;

-- name: CountProjectOwners :one
SELECT count(*)::integer AS owner_count FROM project_memberships
WHERE project_id = @project_id AND role = 'owner';

-- name: ListProjectMembers :many
SELECT u.id AS user_id, u.handle, u.display_name, pm.role, pm.created_at AS joined_at
FROM project_memberships pm
JOIN users u ON u.id = pm.user_id
WHERE pm.project_id = @project_id
ORDER BY pm.created_at, u.id;

-- name: GetProgramByID :one
SELECT * FROM programs WHERE id = @id;

-- name: GetProjectMembership :one
SELECT * FROM project_memberships
WHERE project_id = @project_id AND user_id = @user_id;

-- name: ListProjectsForUser :many
-- Projects the user belongs to (any project membership), most recently
-- created first.
SELECT p.*
FROM projects p
JOIN project_memberships m ON m.project_id = p.id
WHERE m.user_id = @user_id
ORDER BY p.created_at DESC, p.id;

-- name: ListPublicProjects :many
-- Public projects readable by every matrix class (read_public_project,
-- T0106 — anonymous included), most recently created first. The
-- visibility predicate is the read policy: a private row can never reach
-- this result set.
SELECT * FROM projects
WHERE visibility = 'public'
ORDER BY created_at DESC, id;
