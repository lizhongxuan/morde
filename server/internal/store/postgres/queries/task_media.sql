-- name: CreateTaskMediaStagedObject :one
INSERT INTO task_media_staged_objects (
  operation_id, task_id, owner_id, initiator_id, runtime_call_id, media_type, size_bytes,
  sha256, expires_at, state
)
SELECT sqlc.arg(operation_id), tasks.id, tasks.owner_id, tasks.initiator_id, sqlc.arg(runtime_call_id),
  sqlc.arg(media_type), sqlc.arg(size_bytes), sqlc.arg(sha256), sqlc.arg(expires_at), sqlc.arg(state)
FROM tasks
WHERE tasks.id = sqlc.arg(task_id) AND tasks.owner_id = sqlc.arg(owner_id)
RETURNING *;

-- name: LinkTaskMediaStagedObjectToPendingApproval :execrows
UPDATE task_pending_requests
SET media_preview_operation_id = sqlc.arg(operation_id)
WHERE id = sqlc.arg(pending_request_id)
  AND task_id = sqlc.arg(task_id)
  AND version = sqlc.arg(pending_request_version)
  AND status = 'pending'
  AND media_preview_operation_id IS NULL;

-- name: GetTaskMediaOperationDetails :one
SELECT * FROM task_media_operation_details
WHERE task_id = sqlc.arg(task_id) AND operation_id = sqlc.arg(operation_id);

-- name: GetTaskMediaPreviewForOwner :one
SELECT details.*
FROM task_media_operation_details AS details
JOIN task_pending_requests AS pending
  ON pending.task_id = details.task_id AND pending.media_preview_operation_id = details.operation_id
JOIN tasks ON tasks.id = details.task_id AND tasks.owner_id = details.owner_id
WHERE tasks.owner_id = sqlc.arg(owner_id)
  AND tasks.status = 'waiting_user'
  AND details.task_id = sqlc.arg(task_id)
  AND details.operation_id = sqlc.arg(operation_id)
  AND pending.status = 'pending'
  AND details.operation_status = 'succeeded'
  AND details.token_state = 'settled'
  AND details.state = 'preview'
  AND details.cleanup_state = 'pending'
  AND details.expires_at > sqlc.arg(now);

-- name: GetTaskMediaForApprovedSave :one
SELECT details.*
FROM task_media_operation_details AS details
JOIN task_pending_requests AS pending
  ON pending.task_id = details.task_id AND pending.media_preview_operation_id = details.operation_id
JOIN tasks ON tasks.id = details.task_id AND tasks.owner_id = details.owner_id
WHERE tasks.owner_id = sqlc.arg(owner_id)
  AND tasks.status IN ('queued', 'running')
  AND details.task_id = sqlc.arg(task_id)
  AND details.operation_id = sqlc.arg(operation_id)
  AND pending.id = sqlc.arg(pending_request_id)
  AND pending.version = sqlc.arg(pending_request_version)
  AND pending.status = 'approved'
  AND details.operation_status = 'succeeded'
  AND details.token_state = 'settled'
  AND details.input_tokens IS NOT NULL
  AND details.output_tokens IS NOT NULL
  AND details.reserved_tokens > 0
  AND details.input_tokens + details.output_tokens <= details.reserved_tokens
  AND details.state = 'saving'
  AND details.cleanup_state = 'pending';

-- name: ListTaskMediaObjectsForCleanup :many
SELECT * FROM task_media_operation_details
WHERE cleanup_state = 'complete'
   OR (cleanup_state = 'pending'
       AND (state IN ('published', 'discarded', 'expired') OR (state = 'preview' AND expires_at <= sqlc.arg(now))))
ORDER BY expires_at, created_at
LIMIT sqlc.arg(batch_size);

-- name: SetTaskMediaStagedObjectState :one
UPDATE task_media_staged_objects
SET state = sqlc.arg(next_state), updated_at = clock_timestamp()
WHERE task_id = sqlc.arg(task_id) AND operation_id = sqlc.arg(operation_id)
  AND state = sqlc.arg(expected_state) AND cleanup_state = 'pending'
RETURNING *;

-- name: MarkTaskMediaStagedObjectCleanupComplete :execrows
UPDATE task_media_staged_objects
SET cleanup_state = 'complete', updated_at = clock_timestamp()
WHERE task_id = sqlc.arg(task_id) AND operation_id = sqlc.arg(operation_id)
  AND (cleanup_state = 'complete' OR (cleanup_state = 'pending' AND state IN ('published', 'discarded', 'expired')));

-- name: DeleteCleanedTaskMediaStagedObject :exec
DELETE FROM task_media_staged_objects
WHERE task_id = sqlc.arg(task_id) AND operation_id = sqlc.arg(operation_id)
  AND cleanup_state = 'complete';
