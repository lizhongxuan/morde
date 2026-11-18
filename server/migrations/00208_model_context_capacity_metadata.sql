-- +goose Up
ALTER TABLE model_connections
  ADD COLUMN context_window_override_tokens integer,
  ADD COLUMN context_window_source text NOT NULL DEFAULT 'unknown',
  ADD COLUMN context_window_resolved_at timestamptz,
  ADD COLUMN context_window_catalog_version text;

ALTER TABLE model_connections
  DROP CONSTRAINT model_connections_context_window_tokens_check,
  ADD CONSTRAINT model_connections_context_window_tokens_check
    CHECK (context_window_tokens IS NULL OR context_window_tokens BETWEEN 4096 AND 2000000);

UPDATE model_connections
SET context_window_override_tokens = context_window_tokens,
    context_window_source = 'legacy_manual'
WHERE context_window_tokens IS NOT NULL;

UPDATE model_connections AS connection
SET context_window_tokens = catalog.context_window_tokens,
    context_window_override_tokens = NULL,
    context_window_source = 'model_catalog',
    context_window_resolved_at = clock_timestamp(),
    context_window_catalog_version = '2026-11-18'
FROM (VALUES
  ('https://api.openai.com/v1', 'gpt-6-luna', 1050000),
  ('https://api.openai.com/v1', 'gpt-6.1-sol', 1050000),
  ('https://api.openai.com/v1', 'gpt-6-astra', 1050000),
  ('https://api.deepseek.com', 'deepseek-flash', 1048576),
  ('https://api.z.ai/api/paas/v4', 'glm-5.1', 200000)
) AS catalog(base_url, model_name, context_window_tokens)
WHERE lower(rtrim(connection.base_url, '/')) = catalog.base_url
  AND connection.model_name = catalog.model_name;

ALTER TABLE model_connections
  ADD CONSTRAINT model_connections_context_window_override_tokens_check
    CHECK (context_window_override_tokens IS NULL OR context_window_override_tokens BETWEEN 4096 AND 2000000),
  ADD CONSTRAINT model_connections_context_window_source_check
    CHECK (context_window_source IN ('unknown', 'override', 'legacy_manual', 'provider_metadata', 'model_catalog', 'provider_error')),
  ADD CONSTRAINT model_connections_context_window_metadata_check
    CHECK (
      (context_window_source IN ('provider_metadata', 'model_catalog', 'provider_error') AND context_window_resolved_at IS NOT NULL)
      OR (context_window_source IN ('unknown', 'override', 'legacy_manual') AND context_window_resolved_at IS NULL)
    ),
  ADD CONSTRAINT model_connections_context_window_catalog_version_check
    CHECK ((context_window_source = 'model_catalog') = (context_window_catalog_version IS NOT NULL)),
  ADD CONSTRAINT model_connections_context_window_value_source_check
    CHECK (
      (context_window_source = 'unknown' AND context_window_tokens IS NULL AND context_window_override_tokens IS NULL)
      OR (context_window_source IN ('override', 'legacy_manual') AND context_window_tokens = context_window_override_tokens AND context_window_override_tokens IS NOT NULL)
      OR (context_window_source IN ('provider_metadata', 'model_catalog', 'provider_error') AND context_window_tokens IS NOT NULL AND context_window_override_tokens IS NULL)
    );

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM model_connections
    WHERE context_window_tokens > 1000000 OR context_window_override_tokens > 1000000
  ) THEN
    RAISE EXCEPTION 'cannot downgrade model context capacity while a value exceeds 1000000 tokens';
  END IF;
END
$$;
-- +goose StatementEnd

UPDATE model_connections
SET context_window_tokens = context_window_override_tokens
WHERE context_window_source IN ('override', 'legacy_manual') AND context_window_override_tokens IS NOT NULL;

ALTER TABLE model_connections
  DROP CONSTRAINT model_connections_context_window_value_source_check,
  DROP CONSTRAINT model_connections_context_window_catalog_version_check,
  DROP CONSTRAINT model_connections_context_window_metadata_check,
  DROP CONSTRAINT model_connections_context_window_source_check,
  DROP CONSTRAINT model_connections_context_window_override_tokens_check,
  DROP COLUMN context_window_catalog_version,
  DROP COLUMN context_window_resolved_at,
  DROP COLUMN context_window_source,
  DROP COLUMN context_window_override_tokens,
  DROP CONSTRAINT model_connections_context_window_tokens_check,
  ADD CONSTRAINT model_connections_context_window_tokens_check
    CHECK (context_window_tokens IS NULL OR context_window_tokens BETWEEN 4096 AND 1000000);
