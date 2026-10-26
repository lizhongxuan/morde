-- name: GetWorkspace :one
SELECT * FROM workspaces WHERE id = $1;

-- name: ListWorkspacesByOwner :many
SELECT * FROM workspaces WHERE owner_id = $1 AND state <> 'deleted' ORDER BY created_at DESC, id;

-- name: CreateWorkspace :one
INSERT INTO workspaces (id, owner_id, name, state, revision, used_bytes, quota_bytes, last_error)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetPendingWorkspaceLegacyVolume :one
SELECT w.id AS workspace_id, v.volume_name, w.revision, w.used_bytes, w.quota_bytes
FROM workspace_legacy_volumes v
JOIN workspaces w ON w.id = v.workspace_id
WHERE v.workspace_id = $1 AND v.import_state = 'pending' AND w.state = 'ready';

-- name: ListPendingWorkspaceLegacyVolumes :many
SELECT w.id AS workspace_id, v.volume_name, w.revision, w.used_bytes, w.quota_bytes
FROM workspace_legacy_volumes v
JOIN workspaces w ON w.id = v.workspace_id
WHERE v.import_state = 'pending' AND w.state = 'ready'
ORDER BY v.created_at, v.workspace_id;

-- name: GetWorkspaceBinding :one
SELECT wb.id AS binding_id, wb.owner_id, wb.host_id, wb.workspace_id,
       wb.directory_name, wb.state AS binding_state, wb.binding_version,
       wb.desired_revision, wb.applied_revision, wb.sync_state, wb.sync_conflicts,
       wb.created_at AS binding_created_at, wb.updated_at AS binding_updated_at,
       w.name AS workspace_name, w.state AS workspace_state, w.revision AS workspace_revision,
       w.used_bytes AS workspace_used_bytes, w.quota_bytes AS workspace_quota_bytes,
       w.last_error AS workspace_last_error, w.created_at AS workspace_created_at,
       w.updated_at AS workspace_updated_at, w.deleted_at AS workspace_deleted_at
FROM workspace_bindings wb
JOIN workspaces w ON w.id = wb.workspace_id
WHERE wb.host_id = $1 AND wb.workspace_id = $2;

-- name: ListWorkspaceBindingsByHost :many
SELECT wb.id AS binding_id, wb.owner_id, wb.host_id, wb.workspace_id,
       wb.directory_name, wb.state AS binding_state, wb.binding_version,
       wb.desired_revision, wb.applied_revision, wb.sync_state, wb.sync_conflicts,
       wb.created_at AS binding_created_at, wb.updated_at AS binding_updated_at,
       w.name AS workspace_name, w.state AS workspace_state, w.revision AS workspace_revision,
       w.used_bytes AS workspace_used_bytes, w.quota_bytes AS workspace_quota_bytes,
       w.last_error AS workspace_last_error, w.created_at AS workspace_created_at,
       w.updated_at AS workspace_updated_at, w.deleted_at AS workspace_deleted_at
FROM workspace_bindings wb
JOIN workspaces w ON w.id = wb.workspace_id
WHERE wb.host_id = $1
ORDER BY wb.created_at, wb.id;

-- name: ListWorkspaceBindingsByHostIDs :many
SELECT wb.id AS binding_id, wb.owner_id, wb.host_id, wb.workspace_id,
       wb.directory_name, wb.state AS binding_state, wb.binding_version,
       wb.desired_revision, wb.applied_revision, wb.sync_state, wb.sync_conflicts,
       wb.created_at AS binding_created_at, wb.updated_at AS binding_updated_at,
       w.name AS workspace_name, w.state AS workspace_state, w.revision AS workspace_revision,
       w.used_bytes AS workspace_used_bytes, w.quota_bytes AS workspace_quota_bytes,
       w.last_error AS workspace_last_error, w.created_at AS workspace_created_at,
       w.updated_at AS workspace_updated_at, w.deleted_at AS workspace_deleted_at
FROM workspace_bindings wb
JOIN workspaces w ON w.id = wb.workspace_id
WHERE wb.host_id = ANY($1::text[])
ORDER BY wb.host_id, wb.created_at, wb.id;

-- name: ListWorkspaceBindingsByWorkspace :many
SELECT wb.id AS binding_id, wb.owner_id, wb.host_id, wb.workspace_id,
       wb.directory_name, wb.state AS binding_state, wb.binding_version,
       wb.desired_revision, wb.applied_revision, wb.sync_state, wb.sync_conflicts,
       wb.created_at AS binding_created_at, wb.updated_at AS binding_updated_at,
       w.name AS workspace_name, w.state AS workspace_state, w.revision AS workspace_revision,
       w.used_bytes AS workspace_used_bytes, w.quota_bytes AS workspace_quota_bytes,
       w.last_error AS workspace_last_error, w.created_at AS workspace_created_at,
       w.updated_at AS workspace_updated_at, w.deleted_at AS workspace_deleted_at
FROM workspace_bindings wb
JOIN workspaces w ON w.id = wb.workspace_id
WHERE wb.workspace_id = $1
ORDER BY wb.created_at, wb.id;

-- name: CreateWorkspaceBinding :one
INSERT INTO workspace_bindings (
  id, owner_id, host_id, workspace_id, directory_name, state,
  binding_version, desired_revision, applied_revision
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;
