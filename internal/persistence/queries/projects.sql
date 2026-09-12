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

-- name: GetProjectBySlug :one
SELECT * FROM projects WHERE organization_id = @organization_id AND slug = @slug;

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

-- name: AddProjectMembership :exec
INSERT INTO project_memberships (project_id, user_id, role)
VALUES (@project_id, @user_id, @role);

-- name: ListProjectMembers :many
SELECT u.id AS user_id, u.handle, u.display_name, pm.role, pm.created_at AS joined_at
FROM project_memberships pm
JOIN users u ON u.id = pm.user_id
WHERE pm.project_id = @project_id
ORDER BY pm.created_at, u.id;
