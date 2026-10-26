-- name: CreateModelConnection :one
INSERT INTO model_connections (
  id, owner_id, name, base_url, model_name, api_key_cipher,
  capability_text, capability_tools, capability_vision, checked_at
) VALUES (
  sqlc.arg(id), sqlc.narg(owner_id), sqlc.arg(name), sqlc.arg(base_url), sqlc.arg(model_name), sqlc.arg(api_key_cipher),
  false, false, false, NULL
)
RETURNING *;

-- name: ListModelConnections :many
SELECT * FROM model_connections
WHERE owner_id IS NULL OR owner_id = sqlc.arg(owner_id)
ORDER BY owner_id NULLS FIRST, lower(name), id;

-- name: GetModelConnection :one
SELECT * FROM model_connections WHERE id = sqlc.arg(id);

-- name: UpdateModelConnection :one
UPDATE model_connections SET
  name = sqlc.arg(name), base_url = sqlc.arg(base_url), model_name = sqlc.arg(model_name),
  api_key_cipher = sqlc.arg(api_key_cipher), capability_text = sqlc.arg(capability_text),
  capability_tools = sqlc.arg(capability_tools), capability_vision = sqlc.arg(capability_vision),
  checked_at = sqlc.narg(checked_at), updated_at = clock_timestamp()
WHERE id = sqlc.arg(id) AND updated_at = sqlc.arg(expected_updated_at)
RETURNING *;

-- name: DeleteModelConnection :one
DELETE FROM model_connections WHERE id = sqlc.arg(id) RETURNING id;
