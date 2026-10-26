-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserByName :one
SELECT * FROM users WHERE name = $1;

-- name: ListUsers :many
SELECT * FROM users ORDER BY created_at DESC, id;

-- name: ListUsersByStatus :many
SELECT * FROM users WHERE status = $1 ORDER BY created_at DESC, id;

-- name: CreateUser :one
INSERT INTO users (id, name, password_hash, role, status, must_change_password, quota_hosts, quota_cpu_milli, quota_mem_mib)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: UpdateUser :one
UPDATE users
SET name = $2, password_hash = $3, role = $4, status = $5,
    must_change_password = $6, quota_hosts = $7,
    quota_cpu_milli = $8, quota_mem_mib = $9, updated_at = now()
WHERE id = $1 AND status = sqlc.arg(expected_status)::text
RETURNING *;

