-- +goose Up
CREATE TABLE mcp_connector_oauth_flows (
  id text PRIMARY KEY CHECK (id ~ '^o[a-zA-Z0-9_-]{24}$'),
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  connector_id text NOT NULL REFERENCES mcp_connectors(id) ON DELETE CASCADE,
  connector_revision bigint NOT NULL CHECK (connector_revision >= 1),
  state_hash bytea NOT NULL UNIQUE CHECK (octet_length(state_hash) = 32),
  payload_cipher bytea NOT NULL CHECK (octet_length(payload_cipher) BETWEEN 29 AND 65536),
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX mcp_connector_oauth_flows_expiry_idx ON mcp_connector_oauth_flows (expires_at);

-- +goose Down
DROP TABLE mcp_connector_oauth_flows;
