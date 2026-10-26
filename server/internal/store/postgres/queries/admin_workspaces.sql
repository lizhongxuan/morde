-- name: ListAdminWorkspaces :many
SELECT w.id, w.owner_id, u.name AS owner_name, w.name, w.state, w.revision,
       w.used_bytes, w.quota_bytes, w.created_at, w.updated_at
FROM workspaces w
JOIN users u ON u.id = w.owner_id
WHERE w.state <> 'deleted' AND w.deleted_at IS NULL
  AND (NOT $1::boolean OR w.created_at < $2 OR (w.created_at = $2 AND w.id < $3))
ORDER BY w.created_at DESC, w.id DESC
LIMIT $4;

-- name: GetAdminWorkspace :one
SELECT w.id, w.owner_id, u.name AS owner_name, w.name, w.state, w.revision,
       w.used_bytes, w.quota_bytes, w.created_at, w.updated_at
FROM workspaces w
JOIN users u ON u.id = w.owner_id
WHERE w.id = $1 AND w.state <> 'deleted' AND w.deleted_at IS NULL;

-- name: ListAdminWorkspaceBindings :many
SELECT wb.workspace_id, wb.host_id, h.name AS host_name, wb.state,
       wb.sync_state, wb.desired_revision, wb.applied_revision
FROM workspace_bindings wb
JOIN hosts h ON h.id = wb.host_id
WHERE wb.workspace_id = ANY($1::text[])
ORDER BY wb.workspace_id, wb.created_at, wb.id;

