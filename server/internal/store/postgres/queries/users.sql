-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserByName :one
SELECT * FROM users WHERE name = $1;

-- name: ListUsers :many
SELECT * FROM users ORDER BY created_at DESC, id;

-- name: CreateUser :one
INSERT INTO users (id, name, password_hash, role, quota_hosts, quota_cpu_milli, quota_mem_mib, disabled)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: UpdateUser :one
UPDATE users
SET name = $2, password_hash = $3, role = $4, quota_hosts = $5,
    quota_cpu_milli = $6, quota_mem_mib = $7, disabled = $8, updated_at = now()
WHERE id = $1
RETURNING *;

