-- +goose Up
CREATE TABLE task_mcp_connector_operations (
  id                 text PRIMARY KEY CHECK (id ~ '^o[a-z0-9]{7}$'),
  task_id            text NOT NULL,
  owner_id           text NOT NULL,
  connector_id       text NOT NULL CHECK (connector_id ~ '^k[a-z0-9]{7}$'),
  connector_revision bigint NOT NULL CHECK (connector_revision > 0),
  credential_version bigint NOT NULL CHECK (credential_version >= 0),
  grant_version      bigint NOT NULL CHECK (grant_version > 0),
  lease_version      bigint NOT NULL CHECK (lease_version > 0),
  operation_key      text NOT NULL CHECK (length(operation_key) BETWEEN 1 AND 512),
  tool_name          text NOT NULL CHECK (length(tool_name) BETWEEN 1 AND 128),
  request_hash       bytea NOT NULL CHECK (octet_length(request_hash) = 32),
  status             text NOT NULL CHECK (status IN ('prepared', 'dispatched', 'succeeded', 'failed', 'cancelled', 'unknown')),
  cancel_requested   boolean NOT NULL DEFAULT false,
  result             text NOT NULL DEFAULT '' CHECK (octet_length(result) <= 65536),
  created_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
  UNIQUE (task_id, connector_id, operation_key),
  FOREIGN KEY (task_id, owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE
);

CREATE INDEX task_mcp_connector_operations_active_idx
  ON task_mcp_connector_operations (task_id, status)
  WHERE status IN ('prepared', 'dispatched');

-- +goose Down
DROP TABLE task_mcp_connector_operations;
