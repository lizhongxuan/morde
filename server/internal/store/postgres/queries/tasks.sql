-- name: GetTask :one
SELECT * FROM tasks WHERE id = $1 AND owner_id = $2;

-- name: ListTasks :many
SELECT tasks.* FROM tasks
WHERE ((tasks.project_id IS NULL AND tasks.owner_id = $1) OR (tasks.project_id IS NOT NULL AND EXISTS (
    SELECT 1 FROM project_members pm JOIN users u ON u.id=pm.user_id AND u.status='approved'
    WHERE pm.project_id=tasks.project_id AND pm.user_id=$1
  )))
  AND NOT EXISTS (SELECT 1 FROM conversation_route_internal_tool_calls internal_tool WHERE internal_tool.task_id = tasks.id)
  AND ($2::text[] IS NULL OR tasks.host_id = ANY($2::text[]))
  AND ($3 = '' OR tasks.status = $3)
  AND ($4::timestamptz IS NULL OR tasks.created_at < $4::timestamptz OR (tasks.created_at = $4::timestamptz AND tasks.id < $5))
ORDER BY tasks.created_at DESC, tasks.id DESC
LIMIT $6;

-- name: ListConversationActiveTasks :many
SELECT tasks.* FROM tasks
WHERE tasks.conversation_id = sqlc.arg(conversation_id)
  AND tasks.project_id IS NOT DISTINCT FROM sqlc.narg(project_id)::text
  AND ((tasks.project_id IS NULL AND tasks.owner_id = sqlc.arg(actor_id) AND tasks.initiator_id = tasks.owner_id)
    OR (tasks.project_id IS NOT NULL AND EXISTS (
      SELECT 1 FROM project_members pm JOIN users u ON u.id = pm.user_id AND u.status = 'approved'
      WHERE pm.project_id = tasks.project_id AND pm.user_id = sqlc.arg(actor_id)
    )))
  AND NOT EXISTS (SELECT 1 FROM conversation_route_internal_tool_calls internal_tool WHERE internal_tool.task_id = tasks.id)
  AND tasks.status IN ('queued', 'running', 'waiting_user', 'pausing', 'paused', 'cancelling')
  AND (sqlc.narg(host_ids)::text[] IS NULL OR tasks.host_id = ANY(sqlc.narg(host_ids)::text[]))
  AND (sqlc.narg(cursor_at)::timestamptz IS NULL OR tasks.created_at < sqlc.narg(cursor_at)::timestamptz
    OR (tasks.created_at = sqlc.narg(cursor_at)::timestamptz AND tasks.id < sqlc.arg(cursor_id)))
ORDER BY tasks.created_at DESC, tasks.id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListConversationTasks :many
SELECT tasks.* FROM tasks
WHERE tasks.conversation_id = sqlc.arg(conversation_id)
  AND tasks.project_id IS NOT DISTINCT FROM sqlc.narg(project_id)::text
  AND ((tasks.project_id IS NULL AND tasks.owner_id = sqlc.arg(actor_id) AND tasks.initiator_id = tasks.owner_id)
    OR (tasks.project_id IS NOT NULL AND EXISTS (
      SELECT 1 FROM project_members pm JOIN users u ON u.id = pm.user_id AND u.status = 'approved'
      WHERE pm.project_id = tasks.project_id AND pm.user_id = sqlc.arg(actor_id)
    )))
  AND NOT EXISTS (SELECT 1 FROM conversation_route_internal_tool_calls internal_tool WHERE internal_tool.task_id = tasks.id)
  AND ((sqlc.arg(task_view)::text = 'active' AND tasks.status IN ('queued', 'running', 'waiting_user', 'pausing', 'paused', 'cancelling'))
    OR (sqlc.arg(task_view)::text = 'recent' AND tasks.status IN ('completed', 'failed', 'cancelled'))
    OR (sqlc.arg(task_view)::text = 'panel' AND tasks.status IN ('queued', 'running', 'waiting_user', 'pausing', 'paused', 'cancelling', 'completed', 'failed', 'cancelled')))
  AND (sqlc.narg(host_ids)::text[] IS NULL OR tasks.host_id = ANY(sqlc.narg(host_ids)::text[]))
  AND ((sqlc.arg(task_view)::text = 'panel' AND (
      sqlc.narg(cursor_at)::timestamptz IS NULL
      OR CASE WHEN tasks.status = 'running' THEN 0 ELSE 1 END > sqlc.arg(cursor_rank)::integer
      OR (CASE WHEN tasks.status = 'running' THEN 0 ELSE 1 END = sqlc.arg(cursor_rank)::integer AND
        (tasks.updated_at < sqlc.narg(cursor_at)::timestamptz OR
          (tasks.updated_at = sqlc.narg(cursor_at)::timestamptz AND tasks.id < sqlc.arg(cursor_id))))
    )) OR (sqlc.arg(task_view)::text <> 'panel' AND (
      sqlc.narg(cursor_at)::timestamptz IS NULL OR tasks.created_at < sqlc.narg(cursor_at)::timestamptz
      OR (tasks.created_at = sqlc.narg(cursor_at)::timestamptz AND tasks.id < sqlc.arg(cursor_id))
    )))
ORDER BY CASE WHEN sqlc.arg(task_view)::text = 'panel' AND tasks.status = 'running' THEN 0 ELSE 1 END,
         CASE WHEN sqlc.arg(task_view)::text = 'panel' THEN tasks.updated_at ELSE tasks.created_at END DESC,
         tasks.id DESC
LIMIT sqlc.arg(page_limit);

-- name: CountConversationPendingTaskHosts :many
SELECT tasks.host_id, count(*)::bigint AS task_count FROM tasks
WHERE tasks.conversation_id = sqlc.arg(conversation_id)
  AND tasks.project_id IS NOT DISTINCT FROM sqlc.narg(project_id)::text
  AND ((tasks.project_id IS NULL AND tasks.owner_id = sqlc.arg(actor_id) AND tasks.initiator_id = tasks.owner_id)
    OR (tasks.project_id IS NOT NULL AND EXISTS (
      SELECT 1 FROM project_members pm JOIN users u ON u.id = pm.user_id AND u.status = 'approved'
      WHERE pm.project_id = tasks.project_id AND pm.user_id = sqlc.arg(actor_id)
    )))
  AND NOT EXISTS (SELECT 1 FROM conversation_route_internal_tool_calls internal_tool WHERE internal_tool.task_id = tasks.id)
  AND tasks.status = 'waiting_user'
  AND (sqlc.narg(host_ids)::text[] IS NULL OR tasks.host_id = ANY(sqlc.narg(host_ids)::text[]))
GROUP BY tasks.host_id;

-- name: ListTaskApprovals :many
SELECT pending.id, pending.task_id, pending.version, pending.kind, pending.title, pending.object,
       pending.content, pending.impact, COALESCE(pending.media_preview_operation_id, '') AS media_preview_operation_id,
       pending.status, pending.decision, pending.created_at, pending.expires_at, pending.responded_at,
       COALESCE(pending.evidence_pointer, '') AS evidence_pointer,
       pending.this_cost_micros, pending.total_cost_micros,
       COALESCE(pending.trigger_rule, '') AS trigger_rule,
       COALESCE(pending.rejection_consequence, '') AS rejection_consequence,
       tasks.conversation_id, COALESCE(tasks.project_id, '') AS project_id, COALESCE(tasks.host_id, '') AS host_id,
       tasks.status AS task_status, tasks.event_seq AS task_event_seq, tasks.updated_at AS task_updated_at
FROM task_pending_requests pending
JOIN tasks ON tasks.id = pending.task_id
WHERE ((tasks.project_id IS NULL AND tasks.owner_id = sqlc.arg(actor_id) AND tasks.initiator_id = tasks.owner_id)
    OR (tasks.project_id IS NOT NULL AND EXISTS (
      SELECT 1 FROM project_members pm JOIN users u ON u.id = pm.user_id AND u.status = 'approved'
      WHERE pm.project_id = tasks.project_id AND pm.user_id = sqlc.arg(actor_id)
    )))
  AND NOT EXISTS (SELECT 1 FROM conversation_route_internal_tool_calls internal_tool WHERE internal_tool.task_id = tasks.id)
  AND (sqlc.narg(host_ids)::text[] IS NULL OR tasks.host_id = ANY(sqlc.narg(host_ids)::text[]))
  AND (sqlc.arg(conversation_id)::text = '' OR tasks.conversation_id = sqlc.arg(conversation_id))
  AND (sqlc.narg(cursor_at)::timestamptz IS NULL OR pending.created_at < sqlc.narg(cursor_at)::timestamptz
    OR (pending.created_at = sqlc.narg(cursor_at)::timestamptz AND pending.id < sqlc.arg(cursor_id)))
ORDER BY pending.created_at DESC, pending.id DESC
LIMIT sqlc.arg(page_limit);

-- name: CountPendingTaskApprovalHosts :many
SELECT COALESCE(tasks.host_id, '') AS host_id, count(*)::bigint AS task_count
FROM task_pending_requests pending
JOIN tasks ON tasks.id = pending.task_id
WHERE pending.status = 'pending' AND (pending.expires_at IS NULL OR pending.expires_at > now())
  AND NOT EXISTS (SELECT 1 FROM conversation_route_internal_tool_calls internal_tool WHERE internal_tool.task_id = tasks.id)
  AND ((tasks.project_id IS NULL AND tasks.owner_id = sqlc.arg(actor_id) AND tasks.initiator_id = tasks.owner_id)
    OR (tasks.project_id IS NOT NULL AND EXISTS (
      SELECT 1 FROM project_members pm JOIN users u ON u.id = pm.user_id AND u.status = 'approved'
      WHERE pm.project_id = tasks.project_id AND pm.user_id = sqlc.arg(actor_id)
    )))
  AND (sqlc.narg(host_ids)::text[] IS NULL OR tasks.host_id = ANY(sqlc.narg(host_ids)::text[]))
  AND (sqlc.arg(conversation_id)::text = '' OR tasks.conversation_id = sqlc.arg(conversation_id))
GROUP BY COALESCE(tasks.host_id, '');

-- name: ListTaskSteps :many
SELECT * FROM task_steps WHERE task_id = $1 ORDER BY sequence;

-- name: ListTaskBatchItems :many
SELECT * FROM task_batch_items WHERE task_id = $1 ORDER BY created_at, id;

-- name: ListTaskArtifacts :many
SELECT id, task_id, step_id, label, path, media_type, created_at, batch_item_id, batch_attempt,
       workspace_id, workspace_revision, size_bytes, sha256
FROM task_artifacts WHERE task_id = $1 ORDER BY created_at, id;

-- name: ListTaskEvents :many
SELECT * FROM task_events
WHERE task_id = $1 AND sequence > $2
ORDER BY sequence
LIMIT $3;
