-- +goose Up
ALTER TABLE task_pending_requests
  ADD COLUMN task_revision bigint CHECK (task_revision IS NULL OR task_revision >= 0),
  ADD COLUMN responded_by_user_id text REFERENCES users(id),
  ADD CONSTRAINT task_pending_requests_task_id_id_version_unique UNIQUE (task_id, id, version);

ALTER TABLE task_operations
  ADD COLUMN task_revision bigint CHECK (task_revision IS NULL OR task_revision >= 0),
  ADD COLUMN call_id text CHECK (call_id IS NULL OR char_length(call_id) BETWEEN 1 AND 256),
  ADD COLUMN tool_name text CHECK (tool_name IS NULL OR char_length(tool_name) BETWEEN 1 AND 128),
  ADD COLUMN approval_id text,
  ADD COLUMN approval_version bigint CHECK (approval_version IS NULL OR approval_version > 0),
  ADD COLUMN approval_request_hash bytea CHECK (approval_request_hash IS NULL OR octet_length(approval_request_hash) = 32),
  ADD CONSTRAINT task_operations_approval_binding_check CHECK (
    (approval_id IS NULL AND approval_version IS NULL AND approval_request_hash IS NULL) OR
    (approval_id IS NOT NULL AND approval_version IS NOT NULL AND approval_request_hash IS NOT NULL AND
     task_revision IS NOT NULL AND call_id IS NOT NULL AND tool_name IS NOT NULL)
  ),
  ADD CONSTRAINT task_operations_approval_pending_fk
    FOREIGN KEY (task_id, approval_id, approval_version)
    REFERENCES task_pending_requests(task_id, id, version) ON DELETE CASCADE;

CREATE UNIQUE INDEX task_operations_approval_binding_unique
  ON task_operations(task_id, approval_id, approval_version)
  WHERE approval_id IS NOT NULL;

ALTER TABLE task_mcp_connector_operations
  ADD COLUMN task_revision bigint CHECK (task_revision IS NULL OR task_revision >= 0),
  ADD COLUMN call_id text CHECK (call_id IS NULL OR char_length(call_id) BETWEEN 1 AND 256),
  ADD COLUMN approval_id text,
  ADD COLUMN approval_version bigint CHECK (approval_version IS NULL OR approval_version > 0),
  ADD COLUMN approval_request_hash bytea CHECK (approval_request_hash IS NULL OR octet_length(approval_request_hash) = 32),
  ADD CONSTRAINT task_mcp_connector_operations_approval_binding_check CHECK (
    (approval_id IS NULL AND approval_version IS NULL AND approval_request_hash IS NULL) OR
    (approval_id IS NOT NULL AND approval_version IS NOT NULL AND approval_request_hash IS NOT NULL AND
     task_revision IS NOT NULL AND call_id IS NOT NULL)
  ),
  ADD CONSTRAINT task_mcp_connector_operations_approval_pending_fk
    FOREIGN KEY (task_id, approval_id, approval_version)
    REFERENCES task_pending_requests(task_id, id, version) ON DELETE CASCADE;

CREATE UNIQUE INDEX task_mcp_connector_operations_approval_binding_unique
  ON task_mcp_connector_operations(task_id, approval_id, approval_version)
  WHERE approval_id IS NOT NULL;

ALTER TABLE task_workspace_publish_receipts
  ADD COLUMN task_revision bigint CHECK (task_revision IS NULL OR task_revision >= 0),
  ADD COLUMN call_id text CHECK (call_id IS NULL OR char_length(call_id) BETWEEN 1 AND 256),
  ADD COLUMN tool_name text CHECK (tool_name IS NULL OR char_length(tool_name) BETWEEN 1 AND 128),
  ADD COLUMN approval_id text,
  ADD COLUMN approval_version bigint CHECK (approval_version IS NULL OR approval_version > 0),
  ADD COLUMN approval_request_hash bytea CHECK (approval_request_hash IS NULL OR octet_length(approval_request_hash) = 32),
  ADD CONSTRAINT task_workspace_publish_approval_binding_check CHECK (
    (approval_id IS NULL AND approval_version IS NULL AND approval_request_hash IS NULL) OR
    (approval_id IS NOT NULL AND approval_version IS NOT NULL AND approval_request_hash IS NOT NULL AND
     task_revision IS NOT NULL AND call_id IS NOT NULL AND tool_name IS NOT NULL)
  ),
  ADD CONSTRAINT task_workspace_publish_approval_pending_fk
    FOREIGN KEY (task_id, approval_id, approval_version)
    REFERENCES task_pending_requests(task_id, id, version) ON DELETE CASCADE;

CREATE UNIQUE INDEX task_workspace_publish_approval_binding_unique
  ON task_workspace_publish_receipts(task_id, approval_id, approval_version)
  WHERE approval_id IS NOT NULL;

-- +goose Down
DROP INDEX task_workspace_publish_approval_binding_unique;
ALTER TABLE task_workspace_publish_receipts
  DROP CONSTRAINT task_workspace_publish_approval_pending_fk,
  DROP CONSTRAINT task_workspace_publish_approval_binding_check,
  DROP COLUMN approval_request_hash,
  DROP COLUMN approval_version,
  DROP COLUMN approval_id,
  DROP COLUMN tool_name,
  DROP COLUMN call_id,
  DROP COLUMN task_revision;

DROP INDEX task_mcp_connector_operations_approval_binding_unique;
ALTER TABLE task_mcp_connector_operations
  DROP CONSTRAINT task_mcp_connector_operations_approval_pending_fk,
  DROP CONSTRAINT task_mcp_connector_operations_approval_binding_check,
  DROP COLUMN approval_request_hash,
  DROP COLUMN approval_version,
  DROP COLUMN approval_id,
  DROP COLUMN call_id,
  DROP COLUMN task_revision;

DROP INDEX task_operations_approval_binding_unique;
ALTER TABLE task_operations
  DROP CONSTRAINT task_operations_approval_pending_fk,
  DROP CONSTRAINT task_operations_approval_binding_check,
  DROP COLUMN approval_request_hash,
  DROP COLUMN approval_version,
  DROP COLUMN approval_id,
  DROP COLUMN tool_name,
  DROP COLUMN call_id,
  DROP COLUMN task_revision;

ALTER TABLE task_pending_requests
  DROP CONSTRAINT task_pending_requests_task_id_id_version_unique,
  DROP COLUMN responded_by_user_id,
  DROP COLUMN task_revision;
