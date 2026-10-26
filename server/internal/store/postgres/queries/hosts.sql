-- name: GetHost :one
SELECT * FROM hosts WHERE id = $1;

-- name: ListHostsByOwner :many
SELECT * FROM hosts WHERE owner_id = $1 AND deleted_at IS NULL ORDER BY created_at DESC, id;

-- name: ListHosts :many
SELECT * FROM hosts WHERE deleted_at IS NULL ORDER BY created_at DESC, id;

