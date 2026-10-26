-- +goose Up
ALTER TABLE tokens
  ADD COLUMN host_ids text[] NOT NULL DEFAULT '{}',
  ADD COLUMN scopes text[] NOT NULL DEFAULT '{}',
  ADD CONSTRAINT tokens_host_ids_limit CHECK (cardinality(host_ids) <= 500),
  ADD CONSTRAINT tokens_scope_values CHECK (scopes <@ ARRAY[
    'host:read', 'host:create', 'host:manage', 'host:delete_data',
    'computer:observe', 'computer:operate', 'task:read', 'task:submit', 'task:control'
  ]::text[]),
  ADD CONSTRAINT tokens_grants_api_key_only CHECK (
    kind = 'api_key' OR (cardinality(host_ids) = 0 AND cardinality(scopes) = 0)
  );

-- +goose Down
ALTER TABLE tokens
  DROP CONSTRAINT tokens_grants_api_key_only,
  DROP CONSTRAINT tokens_scope_values,
  DROP CONSTRAINT tokens_host_ids_limit,
  DROP COLUMN scopes,
  DROP COLUMN host_ids;
