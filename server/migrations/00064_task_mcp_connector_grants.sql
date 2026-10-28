-- +goose Up
ALTER TABLE tasks ADD CONSTRAINT tasks_id_owner_unique UNIQUE (id, owner_id);

CREATE TABLE task_mcp_connector_grants (
  task_id             text NOT NULL,
  owner_id            text NOT NULL,
  connector_id        text NOT NULL CHECK (connector_id ~ '^k[a-z0-9]{7}$'),
  connector_revision  bigint NOT NULL CHECK (connector_revision > 0),
  credential_version  bigint NOT NULL CHECK (credential_version >= 0),
  grant_version       bigint NOT NULL DEFAULT 1 CHECK (grant_version > 0),
  tool_schema         bytea,
  tool_schema_sha256  bytea CHECK (tool_schema_sha256 IS NULL OR octet_length(tool_schema_sha256) = 32),
  revoked_at          timestamptz,
  created_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (task_id, connector_id),
  UNIQUE (task_id, owner_id, connector_id),
  FOREIGN KEY (task_id, owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE,
  CHECK ((tool_schema IS NULL) = (tool_schema_sha256 IS NULL))
);

CREATE INDEX task_mcp_connector_grants_owner_task_idx
  ON task_mcp_connector_grants (owner_id, task_id, connector_id);

-- +goose Down
DROP TABLE task_mcp_connector_grants;
ALTER TABLE tasks DROP CONSTRAINT tasks_id_owner_unique;
