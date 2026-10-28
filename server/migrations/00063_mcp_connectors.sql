-- +goose Up
CREATE TABLE mcp_connectors (
  id text PRIMARY KEY CHECK (id ~ '^k[a-z0-9]{7}$'),
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name text NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
  endpoint_url text NOT NULL CHECK (length(endpoint_url) BETWEEN 1 AND 2048),
  enabled boolean NOT NULL DEFAULT false,
  credential_cipher bytea,
  credential_version bigint NOT NULL DEFAULT 0 CHECK (credential_version >= 0),
  revision bigint NOT NULL DEFAULT 1 CHECK (revision >= 1),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  CHECK (
    (credential_cipher IS NULL AND credential_version = 0) OR
    (credential_cipher IS NOT NULL AND octet_length(credential_cipher) BETWEEN 29 AND 32796 AND credential_version > 0)
  )
);

CREATE UNIQUE INDEX mcp_connectors_owner_name_idx ON mcp_connectors (owner_id, lower(name));
CREATE INDEX mcp_connectors_owner_enabled_idx ON mcp_connectors (owner_id, enabled, id);

-- +goose Down
DROP TABLE mcp_connectors;
