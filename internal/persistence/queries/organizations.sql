-- Organizations and memberships (canonical tables: organizations,
-- organization_memberships; 00017 adds organizations.deactivated_at).

-- name: CreateOrganization :one
INSERT INTO organizations (slug, name, description)
VALUES (@slug, @name, @description)
RETURNING *;

-- name: GetOrganizationByID :one
SELECT * FROM organizations WHERE id = @id;

-- name: GetOrganizationByIDForUpdate :one
-- Row-locks the organization: governance writes serialize on this lock, so
-- the last-owner check and the change that depends on it are atomic.
SELECT * FROM organizations WHERE id = @id FOR UPDATE;

-- name: GetOrganizationBySlug :one
SELECT * FROM organizations WHERE slug = @slug;

-- name: ListOrganizations :many
SELECT * FROM organizations
ORDER BY created_at, id
LIMIT @page_size OFFSET @page_offset;

-- name: UpdateOrganization :one
UPDATE organizations
SET name = @name, description = @description
WHERE id = @id
RETURNING *;

-- name: DeactivateOrganization :one
UPDATE organizations
SET deactivated_at = now()
WHERE id = @id
RETURNING *;

-- name: ListOrganizationsForUser :many
-- Organizations the user currently belongs to (open affiliation), most
-- recently created first.
SELECT o.*
FROM organizations o
JOIN organization_memberships m ON m.organization_id = o.id
WHERE m.user_id = @user_id AND m.affiliation_end IS NULL
ORDER BY o.created_at DESC, o.id;

-- name: AddOrganizationMembership :exec
INSERT INTO organization_memberships
    (organization_id, user_id, role, affiliation_start, affiliation_end, verified)
VALUES
    (@organization_id, @user_id, @role, @affiliation_start, @affiliation_end, @verified);

-- name: GetOrganizationMembership :one
SELECT * FROM organization_memberships
WHERE organization_id = @organization_id AND user_id = @user_id;

-- name: ListOrganizationMemberships :many
SELECT * FROM organization_memberships
WHERE organization_id = @organization_id
ORDER BY affiliation_end NULLS FIRST, affiliation_start DESC, user_id;

-- name: UpdateOrganizationMembership :one
-- Adjusts role/affiliation_start/verified. affiliation_end is deliberately
-- NOT a column of this statement: it is written exclusively by
-- EndOrganizationAffiliation, so no adjustment path can clear or re-stamp
-- a departure date (离职不删除历史 — the historical end date survives by
-- construction, whatever the caller passes).
UPDATE organization_memberships
SET role = @role,
    affiliation_start = @affiliation_start,
    verified = @verified
WHERE organization_id = @organization_id AND user_id = @user_id
RETURNING *;

-- name: EndOrganizationAffiliation :one
-- Stamps affiliation_end and keeps the row (离职不删除历史 — history is
-- never deleted, docs/04 §6). The store only executes this for
-- still-open affiliations (it reads the row first under the organization
-- lock): ending an already-ended membership is a no-op, so the historical
-- end date is never re-stamped.
UPDATE organization_memberships
SET affiliation_end = @affiliation_end
WHERE organization_id = @organization_id AND user_id = @user_id
RETURNING *;

-- name: CountActiveOrganizationOwners :one
SELECT count(*) FROM organization_memberships
WHERE organization_id = @organization_id AND role = 'owner' AND affiliation_end IS NULL;
