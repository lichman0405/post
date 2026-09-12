-- Users (canonical table: users).

-- name: CreateUser :one
INSERT INTO users (handle, email, display_name)
VALUES (@handle, @email, @display_name)
RETURNING *;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = @id;

-- name: GetUserByHandle :one
SELECT * FROM users WHERE handle = @handle;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = @email;

-- name: ListUsers :many
SELECT * FROM users
ORDER BY created_at, id
LIMIT @page_size OFFSET @page_offset;
