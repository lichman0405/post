-- Organizations and memberships (canonical tables: organizations,
-- organization_memberships).

-- name: CreateOrganization :one
INSERT INTO organizations (slug, name, description)
VALUES (@slug, @name, @description)
RETURNING *;

-- name: GetOrganizationByID :one
SELECT * FROM organizations WHERE id = @id;

-- name: GetOrganizationBySlug :one
SELECT * FROM organizations WHERE slug = @slug;

-- name: ListOrganizations :many
SELECT * FROM organizations
ORDER BY created_at, id
LIMIT @page_size OFFSET @page_offset;

-- name: AddOrganizationMembership :exec
INSERT INTO organization_memberships
    (organization_id, user_id, role, affiliation_start, affiliation_end)
VALUES
    (@organization_id, @user_id, @role, @affiliation_start, @affiliation_end);
