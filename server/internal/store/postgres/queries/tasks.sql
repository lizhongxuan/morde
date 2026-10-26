-- name: GetTask :one
SELECT * FROM tasks WHERE id = $1 AND owner_id = $2;

-- name: ListTasks :many
SELECT * FROM tasks
WHERE owner_id = $1
  AND ($2::text[] IS NULL OR host_id = ANY($2::text[]))
  AND ($3 = '' OR status = $3)
  AND ($4::timestamptz IS NULL OR created_at < $4::timestamptz OR (created_at = $4::timestamptz AND id < $5))
ORDER BY created_at DESC, id DESC
LIMIT $6;

-- name: ListTaskSteps :many
SELECT * FROM task_steps WHERE task_id = $1 ORDER BY sequence;

-- name: ListTaskBatchItems :many
SELECT * FROM task_batch_items WHERE task_id = $1 ORDER BY created_at, id;

-- name: ListTaskArtifacts :many
SELECT * FROM task_artifacts WHERE task_id = $1 ORDER BY created_at, id;

-- name: ListTaskEvents :many
SELECT * FROM task_events
WHERE task_id = $1 AND sequence > $2
ORDER BY sequence
LIMIT $3;
