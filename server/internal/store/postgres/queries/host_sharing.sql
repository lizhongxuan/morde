-- name: UpsertHostMember :one
INSERT INTO host_members (host_id, owner_id, user_id, permissions)
VALUES ($1, $2, $3, $4)
ON CONFLICT (host_id, user_id) DO UPDATE SET owner_id = EXCLUDED.owner_id,
  permissions = EXCLUDED.permissions, created_at = now(), updated_at = now(), revoked_at = NULL
RETURNING *;

-- name: ListHostMembers :many
SELECT * FROM host_members WHERE host_id = $1 AND owner_id = $2 AND revoked_at IS NULL
ORDER BY created_at, user_id;

-- name: RevokeHostMember :execrows
UPDATE host_members SET revoked_at = now(), updated_at = now()
WHERE host_id = $1 AND owner_id = $2 AND user_id = $3 AND revoked_at IS NULL;

-- name: ListSharedHosts :many
SELECT m.host_id, h.name, h.state, m.permissions
FROM host_members m JOIN hosts h ON h.id = m.host_id AND h.owner_id = m.owner_id
WHERE m.user_id = $1 AND m.revoked_at IS NULL AND h.deleted_at IS NULL AND h.state <> 'deleted'
ORDER BY m.created_at DESC, m.host_id;

-- name: GetHostActorAccess :one
SELECT h.owner_id, u.disabled, COALESCE(m.permissions, '{}'::text[]) AS permissions
FROM hosts h JOIN users u ON u.id = $2
LEFT JOIN host_members m ON m.host_id = h.id AND m.user_id = u.id AND m.revoked_at IS NULL
WHERE h.id = $1 AND h.deleted_at IS NULL AND h.state <> 'deleted';
