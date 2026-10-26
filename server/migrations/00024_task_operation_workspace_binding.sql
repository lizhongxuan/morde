-- +goose Up
ALTER TABLE task_operations
  ADD COLUMN workspace_id text NOT NULL DEFAULT '',
  ADD COLUMN binding_version bigint NOT NULL DEFAULT 0,
  ADD CONSTRAINT task_operations_workspace_binding_check CHECK (
    (workspace_id = '' AND binding_version = 0) OR
    (workspace_id ~ '^w[a-z0-9]{7}$' AND binding_version > 0)
  );

ALTER TABLE mcp_runtime_operations
  ADD COLUMN workspace_id text NOT NULL DEFAULT '',
  ADD COLUMN binding_version bigint NOT NULL DEFAULT 0,
  ADD CONSTRAINT mcp_runtime_operations_workspace_binding_check CHECK (
    (workspace_id = '' AND binding_version = 0) OR
    (workspace_id ~ '^w[a-z0-9]{7}$' AND binding_version > 0)
  );

-- +goose Down
ALTER TABLE mcp_runtime_operations
  DROP CONSTRAINT mcp_runtime_operations_workspace_binding_check,
  DROP COLUMN binding_version,
  DROP COLUMN workspace_id;

ALTER TABLE task_operations
  DROP CONSTRAINT task_operations_workspace_binding_check,
  DROP COLUMN binding_version,
  DROP COLUMN workspace_id;
