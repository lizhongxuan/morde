-- +goose Up
CREATE TABLE model_connections (
  id text PRIMARY KEY CHECK (id ~ '^p[a-z0-9]{7}$'),
  owner_id text REFERENCES users(id) ON DELETE CASCADE,
  name text NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
  base_url text NOT NULL CHECK (length(base_url) BETWEEN 1 AND 2048),
  model_name text NOT NULL CHECK (length(model_name) BETWEEN 1 AND 256),
  api_key_cipher bytea NOT NULL CHECK (octet_length(api_key_cipher) BETWEEN 29 AND 4124),
  capability_text boolean NOT NULL DEFAULT false,
  capability_tools boolean NOT NULL DEFAULT false,
  capability_vision boolean NOT NULL DEFAULT false,
  checked_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  CHECK (checked_at IS NOT NULL OR (NOT capability_text AND NOT capability_tools AND NOT capability_vision))
);

CREATE UNIQUE INDEX model_connections_owner_name_idx ON model_connections (COALESCE(owner_id, ''), lower(name));
CREATE INDEX model_connections_available_idx ON model_connections (owner_id, name);

-- +goose Down
DROP TABLE model_connections;
