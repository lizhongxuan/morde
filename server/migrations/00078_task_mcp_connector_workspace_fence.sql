-- +goose Up
ALTER TABLE task_mcp_connector_operations
  ADD COLUMN host_id text NOT NULL DEFAULT '',
  ADD COLUMN workspace_id text NOT NULL DEFAULT '',
  ADD COLUMN binding_version bigint NOT NULL DEFAULT 0,
  ADD COLUMN input_revision bigint,
  ADD COLUMN workspace_checkpoint_revision bigint;

UPDATE task_mcp_connector_operations o
SET host_id = COALESCE(t.host_id, '')
FROM tasks t
WHERE t.id = o.task_id;

ALTER TABLE task_mcp_connector_operations
  ADD CONSTRAINT task_mcp_connector_operations_host_id_check CHECK (host_id = '' OR host_id ~ '^h[a-z0-9]{7}$'),
  ADD CONSTRAINT task_mcp_connector_operations_workspace_fence_check CHECK (
    (workspace_id = '' AND binding_version = 0 AND input_revision IS NULL AND workspace_checkpoint_revision IS NULL)
    OR
    (host_id ~ '^h[a-z0-9]{7}$' AND workspace_id ~ '^w[a-z0-9]{7}$' AND binding_version > 0 AND input_revision IS NOT NULL
      AND input_revision >= 0 AND workspace_checkpoint_revision IS NOT NULL
      AND workspace_checkpoint_revision >= input_revision)
  ),
  ADD CONSTRAINT task_mcp_connector_operations_input_revision_fkey
    FOREIGN KEY (workspace_id, input_revision) REFERENCES workspace_revisions(workspace_id, revision),
  ADD CONSTRAINT task_mcp_connector_operations_checkpoint_revision_fkey
    FOREIGN KEY (workspace_id, workspace_checkpoint_revision) REFERENCES workspace_revisions(workspace_id, revision);

-- +goose Down
ALTER TABLE task_mcp_connector_operations
  DROP CONSTRAINT task_mcp_connector_operations_checkpoint_revision_fkey,
  DROP CONSTRAINT task_mcp_connector_operations_input_revision_fkey,
  DROP CONSTRAINT task_mcp_connector_operations_workspace_fence_check,
  DROP CONSTRAINT task_mcp_connector_operations_host_id_check,
  DROP COLUMN workspace_checkpoint_revision,
  DROP COLUMN input_revision,
  DROP COLUMN binding_version,
  DROP COLUMN workspace_id,
  DROP COLUMN host_id;
