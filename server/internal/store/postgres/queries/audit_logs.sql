-- name: CreateAuditLog :one
INSERT INTO audit_logs (actor_id, actor_kind, action, target, detail, ip)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListAuditLogs :many
SELECT *
FROM audit_logs
WHERE (sqlc.arg(actor)::text = '' OR actor_id = sqlc.arg(actor)::text)
  AND (sqlc.arg(action)::text = '' OR action = sqlc.arg(action)::text)
  AND (sqlc.arg(target)::text = '' OR target = sqlc.arg(target)::text)
  AND (sqlc.narg(since)::timestamptz IS NULL OR at >= sqlc.narg(since))
ORDER BY at DESC, id DESC
LIMIT sqlc.arg(audit_limit)::int;
