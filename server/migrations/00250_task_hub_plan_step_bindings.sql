-- +goose Up
-- Repair note: migration 00248 was edited (e7e4678, 2026-11-25) to add
-- task_step_id AFTER it had already applied to the live database, so the
-- live task_plan_step_execution_bindings table lacks the column while the
-- constraint rewrite below assumes it. A repair migration numbered after
-- 250 cannot help: goose applies pending migrations in ascending version
-- order, so 250 runs first. This migration heals the drift itself.
-- IF NOT EXISTS keeps it a no-op on fresh databases, where 00248 already
-- added the column; 00251 (same delta) stays as a harmless no-op after it.
ALTER TABLE task_plan_step_execution_bindings
  ADD COLUMN IF NOT EXISTS task_step_id text NOT NULL DEFAULT '' CHECK (task_step_id = '' OR task_step_id ~ '^[A-Za-z0-9][A-Za-z0-9_-]{0,39}$');

ALTER TABLE task_hub_operations
  ADD COLUMN agent_session_id text NOT NULL DEFAULT '',
  ADD COLUMN call_id text NOT NULL DEFAULT '',
  ADD COLUMN tool_call_digest bytea,
  ADD COLUMN plan_task_revision bigint NOT NULL DEFAULT 0 CHECK (plan_task_revision >= 0),
  ADD COLUMN plan_revision bigint NOT NULL DEFAULT 0 CHECK (plan_revision >= 0),
  ADD COLUMN plan_step_id text NOT NULL DEFAULT '',
  ADD CONSTRAINT task_hub_operations_plan_binding_check CHECK (
    (plan_revision = 0 AND plan_task_revision = 0 AND plan_step_id = '' AND agent_session_id = '' AND
      call_id = '' AND tool_call_digest IS NULL) OR
    (plan_revision > 0 AND plan_step_id ~ '^[A-Za-z0-9][A-Za-z0-9_-]{0,39}$' AND
      char_length(agent_session_id) BETWEEN 1 AND 128 AND char_length(call_id) BETWEEN 1 AND 256 AND
      tool_call_digest IS NOT NULL AND octet_length(tool_call_digest) = 32)
  );

ALTER TABLE task_plan_step_execution_bindings
  DROP CONSTRAINT task_plan_step_execution_bindings_execution_kind_check,
  ADD CONSTRAINT task_plan_step_execution_bindings_execution_kind_check
    CHECK (execution_kind IN ('operation', 'mcp_connector', 'child_session', 'read_tool', 'hub_operation')),
  DROP CONSTRAINT task_plan_step_execution_bindings_check,
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
    RAISE EXCEPTION 'cannot discard Task Hub plan step bindings';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE task_plan_step_execution_bindings
  DROP CONSTRAINT task_plan_step_execution_bindings_check,
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
  ),
  DROP CONSTRAINT task_plan_step_execution_bindings_execution_kind_check,
  ADD CONSTRAINT task_plan_step_execution_bindings_execution_kind_check
    CHECK (execution_kind IN ('operation', 'mcp_connector', 'child_session', 'read_tool'));

ALTER TABLE task_hub_operations
  DROP CONSTRAINT task_hub_operations_plan_binding_check,
  DROP COLUMN plan_step_id,
  DROP COLUMN plan_revision,
  DROP COLUMN plan_task_revision,
  DROP COLUMN tool_call_digest,
  DROP COLUMN call_id,
  DROP COLUMN agent_session_id;
