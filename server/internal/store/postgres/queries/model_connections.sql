-- name: CreateModelConnection :one
INSERT INTO model_connections (
  id, owner_id, name, base_url, model_name, context_window_tokens, context_window_override_tokens,
  context_window_source, context_window_resolved_at, context_window_catalog_version, api_key_cipher,
  max_output_tokens, max_output_override_tokens, max_output_source, max_output_resolved_at,
  max_output_catalog_version, max_output_source_url,
  capability_text, capability_tools, capability_vision, checked_at
) VALUES (
  sqlc.arg(id), sqlc.narg(owner_id), sqlc.arg(name), sqlc.arg(base_url), sqlc.arg(model_name), sqlc.narg(context_window_tokens),
  sqlc.narg(context_window_override_tokens), sqlc.arg(context_window_source), sqlc.narg(context_window_resolved_at),
  sqlc.narg(context_window_catalog_version), sqlc.arg(api_key_cipher),
  sqlc.narg(max_output_tokens), sqlc.narg(max_output_override_tokens), sqlc.arg(max_output_source),
  sqlc.narg(max_output_resolved_at), sqlc.narg(max_output_catalog_version), sqlc.narg(max_output_source_url),
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
  context_window_tokens = sqlc.narg(context_window_tokens),
  context_window_override_tokens = sqlc.narg(context_window_override_tokens),
  context_window_source = sqlc.arg(context_window_source),
  context_window_resolved_at = sqlc.narg(context_window_resolved_at),
  context_window_catalog_version = sqlc.narg(context_window_catalog_version),
  max_output_tokens = sqlc.narg(max_output_tokens),
  max_output_override_tokens = sqlc.narg(max_output_override_tokens),
  max_output_source = sqlc.arg(max_output_source),
  max_output_resolved_at = sqlc.narg(max_output_resolved_at),
  max_output_catalog_version = sqlc.narg(max_output_catalog_version),
  max_output_source_url = sqlc.narg(max_output_source_url),
  api_key_cipher = sqlc.arg(api_key_cipher), capability_text = sqlc.arg(capability_text),
  capability_tools = sqlc.arg(capability_tools), capability_vision = sqlc.arg(capability_vision),
  capability_media_image_generation = COALESCE(sqlc.narg(capability_media_image_generation), capability_media_image_generation),
  capability_media_video_generation = COALESCE(sqlc.narg(capability_media_video_generation), capability_media_video_generation),
  capability_media_speech_generation = COALESCE(sqlc.narg(capability_media_speech_generation), capability_media_speech_generation),
  checked_at = sqlc.narg(checked_at), updated_at = clock_timestamp()
WHERE id = sqlc.arg(id) AND updated_at = sqlc.arg(expected_updated_at)
RETURNING *;

-- name: DeleteModelConnection :one
DELETE FROM model_connections WHERE id = sqlc.arg(id) RETURNING id;
