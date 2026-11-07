-- +goose Up
ALTER TABLE task_mcp_connector_operations ALTER COLUMN connector_id DROP NOT NULL;
ALTER TABLE task_mcp_connector_operations ADD COLUMN builtin_service text;
ALTER TABLE task_mcp_connector_operations ADD CONSTRAINT task_mcp_operation_source_check CHECK (
  (connector_id IS NOT NULL AND builtin_service IS NULL) OR
  (connector_id IS NULL AND builtin_service IS NOT NULL AND builtin_service = 'searxng-v2.10.0')
);
ALTER TABLE task_mcp_connector_operations DROP CONSTRAINT task_mcp_connector_operations_task_id_connector_id_operatio_key;
ALTER TABLE task_mcp_connector_operations ADD CONSTRAINT task_mcp_operation_source_key
  UNIQUE NULLS NOT DISTINCT (task_id, connector_id, builtin_service, operation_key);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM task_mcp_connector_operations WHERE builtin_service IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot roll back while built-in MCP operation history exists';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE task_mcp_connector_operations DROP CONSTRAINT task_mcp_operation_source_key;
ALTER TABLE task_mcp_connector_operations ADD CONSTRAINT task_mcp_connector_operations_task_id_connector_id_operatio_key
  UNIQUE (task_id, connector_id, operation_key);
ALTER TABLE task_mcp_connector_operations DROP CONSTRAINT task_mcp_operation_source_check;
ALTER TABLE task_mcp_connector_operations DROP COLUMN builtin_service;
ALTER TABLE task_mcp_connector_operations ALTER COLUMN connector_id SET NOT NULL;
