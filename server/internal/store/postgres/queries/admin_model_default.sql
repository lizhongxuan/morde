-- name: GetAdminModelDefault :one
SELECT model_connection_id, version, updated_at
FROM admin_model_defaults
WHERE id = 1;

-- name: SetAdminModelDefault :one
UPDATE admin_model_defaults
SET model_connection_id = NULLIF(sqlc.arg(model_id)::text, ''),
    version = version + 1,
    updated_at = clock_timestamp()
WHERE id = 1
  AND version = sqlc.arg(expected_version)
  AND (
    sqlc.arg(model_id)::text = ''
    OR EXISTS (
      SELECT 1
      FROM model_connections
      WHERE model_connections.id = sqlc.arg(model_id)::text
        AND model_connections.owner_id IS NULL
        AND model_connections.checked_at IS NOT NULL
        AND model_connections.capability_text
    )
  )
RETURNING model_connection_id, version, updated_at;
