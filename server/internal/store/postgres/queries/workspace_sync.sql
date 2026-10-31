-- name: CreateWorkspaceRevision :exec
INSERT INTO workspace_revisions (workspace_id, revision, file_count, total_bytes)
VALUES ($1, $2, $3, $4);

-- name: CreateWorkspaceTreeRevision :exec
INSERT INTO workspace_revisions (workspace_id, revision, file_count, total_bytes, directory_count)
VALUES ($1, $2, $3, $4, $5);

-- name: CloseWorkspaceRevisionFile :execrows
UPDATE workspace_revision_files SET valid_to_revision=$3
WHERE workspace_id=$1 AND file_path=$2 AND valid_to_revision IS NULL AND valid_from_revision < $3;

-- name: GetWorkspaceRevision :one
SELECT workspace_id, revision, file_count, total_bytes, created_at
FROM workspace_revisions WHERE workspace_id=$1 AND revision=$2;

-- name: GetLatestWorkspaceRevision :one
SELECT wr.workspace_id, wr.revision, wr.file_count, wr.total_bytes, wr.created_at
FROM workspace_revisions wr
WHERE wr.workspace_id=$1
ORDER BY wr.revision DESC LIMIT 1;

-- name: ListWorkspaceRevisionFilesPage :many
SELECT file_path, size_bytes, sha256, mode, chunks
FROM workspace_revision_files
WHERE workspace_id=$1 AND valid_from_revision <= $2 AND (valid_to_revision IS NULL OR valid_to_revision > $2)
  AND ($3::text = '' OR file_path=$3 OR starts_with(file_path, $3 || '/'))
  AND file_path > $4
ORDER BY file_path
LIMIT $5;

-- name: ListWorkspaceRevisionEmptyDirectoriesPage :many
SELECT directory_path
FROM workspace_revision_empty_directories
WHERE workspace_id=$1 AND valid_from_revision <= $2 AND (valid_to_revision IS NULL OR valid_to_revision > $2)
  AND ($3::text = '' OR directory_path=$3 OR starts_with(directory_path, $3 || '/'))
  AND directory_path > $4
ORDER BY directory_path
LIMIT $5;

-- name: GetWorkspaceRevisionFile :one
SELECT file_path, size_bytes, sha256, mode, chunks
FROM workspace_revision_files
WHERE workspace_id=$1 AND valid_from_revision <= $2 AND (valid_to_revision IS NULL OR valid_to_revision > $2) AND file_path=$3
ORDER BY valid_from_revision DESC LIMIT 1;

-- name: ListWorkspaceBlockReferences :many
SELECT DISTINCT f.workspace_id, (chunk->>'sha256')::text AS sha256, (chunk->>'size_bytes')::bigint AS size_bytes
FROM workspace_revision_files f
JOIN workspaces w ON w.id=f.workspace_id AND w.state='ready' AND w.deleted_at IS NULL
CROSS JOIN LATERAL jsonb_array_elements(f.chunks) AS chunk
ORDER BY f.workspace_id, sha256, size_bytes;

-- name: ListWorkspaceIDsForBlockGC :many
SELECT id FROM workspaces
WHERE state='ready' AND deleted_at IS NULL
ORDER BY id;

-- name: ListWorkspaceBlockGCReferences :many
SELECT workspace_id, sha256, size_bytes
FROM (
  SELECT f.workspace_id, (chunk->>'sha256')::text AS sha256, (chunk->>'size_bytes')::bigint AS size_bytes
  FROM workspace_revision_files f
  CROSS JOIN LATERAL jsonb_array_elements(f.chunks) AS chunk
  WHERE f.workspace_id=sqlc.arg(workspace_id)
  UNION
  SELECT s.workspace_id, b.sha256, b.size_bytes
  FROM workspace_sync_sessions s
  JOIN workspace_sync_session_blocks b ON b.workspace_id=s.workspace_id AND b.session_id=s.id
  WHERE s.workspace_id=sqlc.arg(workspace_id)
    AND s.state IN ('planned','transferring','verifying','applying')
    AND b.role='upload'
) AS protected_blocks
ORDER BY sha256, size_bytes;

-- name: ExpireIdleWorkspaceSyncSessions :execrows
UPDATE workspace_sync_sessions
SET state='expired', stage='complete', updated_at=now()
WHERE workspace_id=sqlc.arg(workspace_id)
  AND updated_at < sqlc.arg(cutoff)
  AND state IN ('planned','transferring','verifying','applying');

-- name: ListWorkspaceRevisionFiles :many
SELECT file_path, size_bytes, sha256, mode, chunks
FROM workspace_revision_files
WHERE workspace_id=$1 AND valid_from_revision <= $2 AND (valid_to_revision IS NULL OR valid_to_revision > $2)
ORDER BY file_path;

-- name: InsertWorkspaceRevisionFile :exec
INSERT INTO workspace_revision_files (workspace_id, valid_from_revision, file_path, folded_path, size_bytes, sha256, mode, chunks)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: ListWorkspaceRevisionEmptyDirectories :many
SELECT directory_path
FROM workspace_revision_empty_directories
WHERE workspace_id=$1 AND valid_from_revision <= $2 AND (valid_to_revision IS NULL OR valid_to_revision > $2)
ORDER BY directory_path;

-- name: CloseWorkspaceRevisionEmptyDirectory :execrows
UPDATE workspace_revision_empty_directories SET valid_to_revision=$3
WHERE workspace_id=$1 AND directory_path=$2 AND valid_to_revision IS NULL AND valid_from_revision < $3;

-- name: InsertWorkspaceRevisionEmptyDirectory :exec
INSERT INTO workspace_revision_empty_directories (workspace_id, valid_from_revision, directory_path, folded_path)
VALUES ($1, $2, $3, $4);

-- name: InsertWorkspaceChange :exec
INSERT INTO workspace_changes (workspace_id, revision, file_path, action, file)
VALUES ($1, $2, $3, $4, $5);

-- name: ListWorkspaceChangesPage :many
SELECT revision, file_path, action, file
FROM workspace_changes
WHERE workspace_id=$1 AND (revision > $2 OR (revision=$2 AND file_path > $3))
ORDER BY revision, file_path
LIMIT $4;

-- name: CreateWorkspaceSyncSession :one
INSERT INTO workspace_sync_sessions (
  id, workspace_id, owner_id, replica_id, direction, policy, state, stage,
  base_revision, target_revision, pinned_revision, planned_bytes, confirmed_bytes,
  files, upload_files, download_files, scope, deleted_paths, upload_deleted_paths,
  download_deleted_paths, empty_directories, upload_empty_directories, download_empty_directories,
  deleted_directories, upload_deleted_directories, download_deleted_directories, ack_cursor, conflicts
)
VALUES (
  $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28
)
RETURNING *;

-- name: GetWorkspaceSyncSession :one
SELECT * FROM workspace_sync_sessions WHERE workspace_id=$1 AND id=$2;

-- name: ListDeviceWorkspaceSyncProgress :many
WITH latest AS (
  SELECT DISTINCT ON (s.workspace_id, t.id)
    s.workspace_id, t.id AS device_id, s.id AS session_id, s.direction, s.state, s.stage,
    s.planned_bytes, s.confirmed_bytes, s.updated_at
  FROM workspaces w
  JOIN tokens t ON t.owner_id=w.owner_id AND t.kind='device'
    AND t.revoked_at IS NULL AND t.expires_at > now()
    AND t.scopes=ARRAY['workspace:sync']::text[] AND cardinality(t.workspace_ids)=0
  JOIN client_installations ci ON ci.id=t.client_installation_id AND ci.owner_id=w.owner_id
    AND ci.platform='desktop' AND ci.revoked_at IS NULL AND ci.expires_at>now()
  JOIN workspace_sync_sessions s ON s.workspace_id=w.id AND s.owner_id=w.owner_id
    AND s.replica_id='device:' || t.id
  WHERE w.id=$1 AND w.owner_id=$2 AND w.state='ready' AND w.deleted_at IS NULL
  ORDER BY s.workspace_id, t.id, s.created_at DESC, s.id DESC
)
SELECT latest.workspace_id, latest.device_id, latest.session_id, latest.direction, latest.state, latest.stage,
  latest.planned_bytes, latest.confirmed_bytes,
  actions.total AS download_actions_total, actions.applied AS download_actions_done, latest.updated_at
FROM latest
LEFT JOIN LATERAL (
  SELECT count(*)::bigint AS total, count(*) FILTER (WHERE applied)::bigint AS applied
  FROM workspace_sync_session_download_actions a
  WHERE a.workspace_id=latest.workspace_id AND a.session_id=latest.session_id
) actions ON true
WHERE latest.state IN ('planned','transferring','verifying','applying')
ORDER BY latest.updated_at DESC, latest.device_id;

-- name: LockWorkspaceSyncSession :one
SELECT * FROM workspace_sync_sessions WHERE workspace_id=$1 AND id=$2 FOR UPDATE;

-- name: InsertWorkspaceSyncTransferBlock :exec
INSERT INTO workspace_sync_session_blocks (workspace_id, session_id, sha256, size_bytes, role, received)
VALUES ($1,$2,$3,$4,$5,$6);

-- name: InsertWorkspaceSyncDownloadAction :exec
INSERT INTO workspace_sync_session_download_actions
  (workspace_id, session_id, file_path, action, sha256, size_bytes)
VALUES ($1,$2,$3,$4,$5,$6);

-- name: ListWorkspaceSyncTransferBlocks :many
SELECT sha256, size_bytes, role, received
FROM workspace_sync_session_blocks
WHERE workspace_id=$1 AND session_id=$2
ORDER BY role, sha256;

-- name: ListWorkspaceSyncDownloadActions :many
SELECT file_path, action, sha256, size_bytes, applied
FROM workspace_sync_session_download_actions
WHERE workspace_id=$1 AND session_id=$2
ORDER BY file_path;

-- name: GetWorkspaceSyncTransferBlock :one
SELECT sha256, size_bytes, role, received
FROM workspace_sync_session_blocks
WHERE workspace_id=$1 AND session_id=$2 AND sha256=$3 AND role=$4;

-- name: MarkWorkspaceSyncBlockReceived :one
UPDATE workspace_sync_session_blocks
SET received=true
WHERE workspace_id=$1 AND session_id=$2 AND sha256=$3 AND role='upload' AND received=false
RETURNING size_bytes;

-- name: AddWorkspaceSyncConfirmedBytes :exec
UPDATE workspace_sync_sessions
SET confirmed_bytes=confirmed_bytes+$3,
    state=CASE
      WHEN EXISTS (
        SELECT 1 FROM workspace_sync_session_blocks b
        WHERE b.workspace_id=$1 AND b.session_id=$2 AND b.received=false
      ) THEN 'transferring'
      WHEN EXISTS (
        SELECT 1 FROM workspace_sync_session_download_actions a
        WHERE a.workspace_id=$1 AND a.session_id=$2 AND a.applied=false
      ) THEN 'verifying'
      WHEN EXISTS (
        SELECT 1 FROM workspace_sync_session_download_actions a
        WHERE a.workspace_id=$1 AND a.session_id=$2
      ) THEN 'applying'
      ELSE 'verifying'
    END,
    stage=CASE
      WHEN EXISTS (
        SELECT 1 FROM workspace_sync_session_blocks b
        WHERE b.workspace_id=$1 AND b.session_id=$2 AND b.received=false
      ) THEN 'transfer'
      WHEN EXISTS (
        SELECT 1 FROM workspace_sync_session_download_actions a
        WHERE a.workspace_id=$1 AND a.session_id=$2 AND a.applied=false
      ) THEN 'verify'
      WHEN EXISTS (
        SELECT 1 FROM workspace_sync_session_download_actions a
        WHERE a.workspace_id=$1 AND a.session_id=$2
      ) THEN 'apply'
      ELSE 'verify'
    END,
    updated_at=now()
WHERE workspace_id=$1 AND id=$2 AND state IN ('planned','transferring','verifying','applying');

-- name: CancelWorkspaceSyncSession :one
UPDATE workspace_sync_sessions
SET state='cancelled', stage='complete', updated_at=now()
WHERE workspace_id=$1 AND id=$2 AND state IN ('planned','transferring','verifying','applying')
RETURNING *;

-- name: SetWorkspaceSyncSessionState :exec
UPDATE workspace_sync_sessions
SET state=$3, stage=$4, conflicts=$5, target_revision=$6, updated_at=now()
WHERE workspace_id=$1 AND id=$2;

-- name: InsertWorkspaceSyncCursor :exec
INSERT INTO workspace_sync_cursors (workspace_id, owner_id, replica_id, revision, file_path)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (workspace_id, replica_id) DO UPDATE SET
  owner_id=EXCLUDED.owner_id, revision=EXCLUDED.revision, file_path=EXCLUDED.file_path, updated_at=now()
WHERE (workspace_sync_cursors.revision, workspace_sync_cursors.file_path)
  <= (EXCLUDED.revision, EXCLUDED.file_path);

-- name: GetWorkspaceSyncCursor :one
SELECT workspace_id, owner_id, replica_id, revision, file_path, updated_at
FROM workspace_sync_cursors WHERE workspace_id=$1 AND replica_id=$2;

-- name: WorkspaceHasActiveSyncSession :one
SELECT EXISTS (
  SELECT 1 FROM workspace_sync_sessions
  WHERE workspace_id=$1 AND state IN ('planned','transferring','verifying','applying')
);
