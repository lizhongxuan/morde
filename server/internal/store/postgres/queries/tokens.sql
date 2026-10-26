-- name: GetToken :one
SELECT * FROM tokens WHERE id = $1;

-- name: ListTokensByOwner :many
SELECT * FROM tokens WHERE owner_id = $1 ORDER BY created_at DESC, id;

-- name: CreateToken :one
INSERT INTO tokens (id, owner_id, kind, host_id, preview_port, host_ids, workspace_ids, scopes, name, secret_hash, expires_at, used_at, last_used_at, revoked_at, actor_user_id, client_installation_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
RETURNING *;

-- name: RevokeToken :execrows
UPDATE tokens SET revoked_at = COALESCE(revoked_at, now())
WHERE id = $1 AND owner_id = $2;

