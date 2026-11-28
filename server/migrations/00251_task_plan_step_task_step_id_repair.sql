-- +goose Up
-- Repair for migration 00248: the 00248 file was edited (e7e4678, 2026-11-25)
-- to add task_step_id AFTER 00248 had already applied to the live database
-- in the 7af64095 deploy, when it only added result_digest. Goose never
-- re-runs an applied migration, so the live schema drifted: the column is
-- missing while migration 00250 assumes it exists, and the live deploy of
-- a00cd2d aborted on `column "task_step_id" does not exist`. This repair
-- applies the missing delta. IF NOT EXISTS keeps it a no-op on fresh
-- databases, where 00248 already added the column; the check replacement
-- is the exact 00248 definition, so it is identical on both.
ALTER TABLE task_plan_step_execution_bindings
  ADD COLUMN IF NOT EXISTS task_step_id text NOT NULL DEFAULT '' CHECK (task_step_id = '' OR task_step_id ~ '^[A-Za-z0-9][A-Za-z0-9_-]{0,39}$');

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

-- +goose Down
-- Keep the schema on rollback: migration 00250 depends on task_step_id, and
-- older code that reads the 00248 contract keeps working with the column present.
SELECT 1;
