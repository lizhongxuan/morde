-- +goose Up
ALTER TABLE task_plan_step_execution_bindings
  ADD COLUMN result_digest bytea CHECK (result_digest IS NULL OR octet_length(result_digest) = 32);

ALTER TABLE task_plan_step_execution_bindings
  DROP CONSTRAINT task_plan_step_execution_bindings_execution_kind_check,
  ADD CONSTRAINT task_plan_step_execution_bindings_execution_kind_check
    CHECK (execution_kind IN ('operation', 'mcp_connector', 'child_session', 'read_tool'));

ALTER TABLE task_plan_step_execution_bindings
  DROP CONSTRAINT task_plan_step_execution_bindings_check,
  ADD CONSTRAINT task_plan_step_execution_bindings_check CHECK (
    (execution_kind = 'operation' AND tool_name <> '' AND connector_id = '' AND
      child_step_id = '' AND parent_step_id = '' AND result_digest IS NULL) OR
    (execution_kind = 'mcp_connector' AND tool_name <> '' AND connector_id <> '' AND
      child_step_id = '' AND parent_step_id = '' AND result_digest IS NULL) OR
    (execution_kind = 'child_session' AND tool_name = '' AND connector_id = '' AND
      child_step_id <> '' AND execution_id <> agent_session_id AND result_digest IS NULL) OR
    (execution_kind = 'read_tool' AND tool_name <> '' AND connector_id = '' AND
      child_step_id = '' AND parent_step_id = '')
  );

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM task_plan_step_execution_bindings WHERE execution_kind = 'read_tool') THEN
    RAISE EXCEPTION 'cannot discard task plan read tool bindings';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE task_plan_step_execution_bindings
  DROP CONSTRAINT task_plan_step_execution_bindings_check,
  ADD CONSTRAINT task_plan_step_execution_bindings_check CHECK (
    (execution_kind = 'operation' AND tool_name <> '' AND connector_id = '' AND
      child_step_id = '' AND parent_step_id = '') OR
    (execution_kind = 'mcp_connector' AND tool_name <> '' AND connector_id <> '' AND
      child_step_id = '' AND parent_step_id = '') OR
    (execution_kind = 'child_session' AND tool_name = '' AND connector_id = '' AND
      child_step_id <> '' AND execution_id <> agent_session_id)
  );

ALTER TABLE task_plan_step_execution_bindings
  DROP CONSTRAINT task_plan_step_execution_bindings_execution_kind_check,
  ADD CONSTRAINT task_plan_step_execution_bindings_execution_kind_check
    CHECK (execution_kind IN ('operation', 'mcp_connector', 'child_session')),
  DROP COLUMN result_digest;
