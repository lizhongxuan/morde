-- +goose Up
-- Migration 00251 recreated this constraint from the pre-Hub-operation
-- definition in 00248, removing the hub_operation case added by 00250.
-- Restore that case so a dispatched Hub command can be atomically bound to
-- its plan step.
ALTER TABLE task_plan_step_execution_bindings
  DROP CONSTRAINT IF EXISTS task_plan_step_execution_bindings_check,
  ADD CONSTRAINT task_plan_step_execution_bindings_check CHECK (
    (execution_kind = 'operation' AND tool_name <> '' AND connector_id = '' AND
      child_step_id = '' AND parent_step_id = '' AND result_digest IS NULL AND task_step_id = '') OR
    (execution_kind = 'mcp_connector' AND tool_name <> '' AND connector_id <> '' AND
      child_step_id = '' AND parent_step_id = '' AND result_digest IS NULL AND task_step_id = '') OR
    (execution_kind = 'child_session' AND tool_name = '' AND connector_id = '' AND
      child_step_id <> '' AND execution_id <> agent_session_id AND result_digest IS NULL AND task_step_id = '') OR
    (execution_kind = 'read_tool' AND tool_name <> '' AND connector_id = '' AND
      child_step_id = '' AND parent_step_id = '' AND
      ((task_step_id = '' AND result_digest IS NULL) OR (task_step_id <> '' AND result_digest IS NOT NULL))) OR
    (execution_kind = 'hub_operation' AND tool_name <> '' AND connector_id = '' AND
      child_step_id = '' AND parent_step_id = '' AND result_digest IS NULL AND task_step_id = '')
  );

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM task_plan_step_execution_bindings WHERE execution_kind = 'hub_operation') OR
     EXISTS (SELECT 1 FROM task_hub_operations WHERE plan_revision > 0) THEN
    RAISE EXCEPTION 'cannot discard Hub plan step bindings';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE task_plan_step_execution_bindings
  DROP CONSTRAINT IF EXISTS task_plan_step_execution_bindings_check,
  ADD CONSTRAINT task_plan_step_execution_bindings_check CHECK (
    (execution_kind = 'operation' AND tool_name <> '' AND connector_id = '' AND
      child_step_id = '' AND parent_step_id = '' AND result_digest IS NULL AND task_step_id = '') OR
    (execution_kind = 'mcp_connector' AND tool_name <> '' AND connector_id <> '' AND
      child_step_id = '' AND parent_step_id = '' AND result_digest IS NULL AND task_step_id = '') OR
    (execution_kind = 'child_session' AND tool_name = '' AND connector_id = '' AND
      child_step_id <> '' AND execution_id <> agent_session_id AND result_digest IS NULL AND task_step_id = '') OR
    (execution_kind = 'read_tool' AND tool_name <> '' AND connector_id = '' AND
      child_step_id = '' AND parent_step_id = '' AND
      ((task_step_id = '' AND result_digest IS NULL) OR (task_step_id <> '' AND result_digest IS NOT NULL)))
  );
