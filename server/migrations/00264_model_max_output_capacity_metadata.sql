-- +goose Up
ALTER TABLE model_connections
  ADD COLUMN max_output_tokens integer,
  ADD COLUMN max_output_override_tokens integer,
  ADD COLUMN max_output_source text NOT NULL DEFAULT 'unknown',
  ADD COLUMN max_output_resolved_at timestamptz,
  ADD COLUMN max_output_catalog_version text,
  ADD COLUMN max_output_source_url text;

UPDATE model_connections AS connection
SET max_output_tokens = catalog.max_output_tokens,
    max_output_source = 'model_catalog',
    max_output_resolved_at = clock_timestamp(),
    max_output_catalog_version = '2026-11-27',
    max_output_source_url = catalog.source_url
FROM (VALUES
  ('https://api.openai.com/v1', 'gpt-6-luna', 128000, 'https://developers.openai.com/api/docs/models/gpt-6-luna'),
  ('https://api.openai.com/v1', 'gpt-6.1-sol', 128000, 'https://developers.openai.com/api/docs/models/gpt-6.1-sol'),
  ('https://api.openai.com/v1', 'gpt-6-astra', 128000, 'https://developers.openai.com/api/docs/models/gpt-6-astra'),
  ('https://api.deepseek.com', 'deepseek-flash', 393216, 'https://api-docs.deepseek.com/api/create-chat-completion/')
) AS catalog(base_url, model_name, max_output_tokens, source_url)
WHERE lower(rtrim(connection.base_url, '/')) = catalog.base_url
  AND connection.model_name = catalog.model_name;

ALTER TABLE model_connections
  ADD CONSTRAINT model_connections_max_output_tokens_check
    CHECK (max_output_tokens IS NULL OR max_output_tokens BETWEEN 1 AND 2000000),
  ADD CONSTRAINT model_connections_max_output_override_tokens_check
    CHECK (max_output_override_tokens IS NULL OR max_output_override_tokens BETWEEN 1 AND 2000000),
  ADD CONSTRAINT model_connections_max_output_source_check
    CHECK (max_output_source IN ('unknown', 'override', 'model_catalog')),
  ADD CONSTRAINT model_connections_max_output_metadata_check
    CHECK (
      (max_output_source = 'model_catalog' AND max_output_resolved_at IS NOT NULL AND
        max_output_catalog_version IS NOT NULL AND length(max_output_catalog_version) BETWEEN 1 AND 64 AND
        max_output_source_url IS NOT NULL AND length(max_output_source_url) BETWEEN 1 AND 2048 AND max_output_source_url LIKE 'https://%')
      OR (max_output_source IN ('unknown', 'override') AND max_output_resolved_at IS NULL AND
        max_output_catalog_version IS NULL AND max_output_source_url IS NULL)
    ),
  ADD CONSTRAINT model_connections_max_output_value_source_check
    CHECK (
      (max_output_source = 'unknown' AND max_output_tokens IS NULL AND max_output_override_tokens IS NULL)
      OR (max_output_source = 'override' AND max_output_tokens = max_output_override_tokens AND max_output_override_tokens IS NOT NULL)
      OR (max_output_source = 'model_catalog' AND max_output_tokens IS NOT NULL AND max_output_override_tokens IS NULL)
    );

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM model_connections WHERE max_output_source = 'override' AND max_output_override_tokens IS NOT NULL
  ) THEN
    RAISE EXCEPTION 'cannot downgrade model max output capacity while custom overrides are saved';
  END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE model_connections
  DROP CONSTRAINT model_connections_max_output_value_source_check,
  DROP CONSTRAINT model_connections_max_output_metadata_check,
  DROP CONSTRAINT model_connections_max_output_source_check,
  DROP CONSTRAINT model_connections_max_output_override_tokens_check,
  DROP CONSTRAINT model_connections_max_output_tokens_check,
  DROP COLUMN max_output_source_url,
  DROP COLUMN max_output_catalog_version,
  DROP COLUMN max_output_resolved_at,
  DROP COLUMN max_output_source,
  DROP COLUMN max_output_override_tokens,
  DROP COLUMN max_output_tokens;
