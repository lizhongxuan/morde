-- +goose Up
ALTER TABLE task_agent_instances
  ADD COLUMN host_id text,
  ADD COLUMN host_owner_id text,
  ADD COLUMN host_actor_id text,
  ADD COLUMN host_permission text,
  ADD COLUMN host_authorization_kind text,
  ADD COLUMN host_runtime_generation bigint,
  ADD COLUMN host_runtime_instance_id text,
  ADD COLUMN host_control_version bigint,
  ADD CONSTRAINT task_agent_host_fence_check CHECK (
    (
      host_id IS NULL AND host_owner_id IS NULL AND host_actor_id IS NULL AND
      host_permission IS NULL AND host_authorization_kind IS NULL AND
      host_runtime_generation IS NULL AND host_runtime_instance_id IS NULL AND
      host_control_version IS NULL
    ) OR (
      host_id IS NOT NULL AND length(host_id) BETWEEN 1 AND 128 AND
      host_owner_id IS NOT NULL AND host_owner_id <> '' AND
      host_actor_id IS NOT NULL AND host_actor_id <> '' AND
      host_permission = 'desktop:control' AND
      host_authorization_kind IN ('owner', 'member') AND
      host_runtime_generation IS NOT NULL AND host_runtime_generation > 0 AND
      host_runtime_instance_id IS NOT NULL AND length(host_runtime_instance_id) BETWEEN 1 AND 128 AND
      host_control_version IS NOT NULL AND host_control_version >= 0
    )
  ),
  ADD CONSTRAINT task_agent_host_task_fkey
    FOREIGN KEY (task_id, host_id) REFERENCES tasks(id, host_id) ON DELETE CASCADE;

-- +goose Down
-- Existing host fences are required to renew a Host-bound Agent safely.
-- Refuse rollback instead of silently removing those authorization snapshots.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM task_agent_instances WHERE host_id IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot roll back Agent Host fences while Host-bound Agent records exist';
  END IF;
END;
$$;
-- +goose StatementEnd
ALTER TABLE task_agent_instances
  DROP CONSTRAINT task_agent_host_task_fkey,
  DROP CONSTRAINT task_agent_host_fence_check,
  DROP COLUMN host_control_version,
  DROP COLUMN host_runtime_instance_id,
  DROP COLUMN host_runtime_generation,
  DROP COLUMN host_authorization_kind,
  DROP COLUMN host_permission,
  DROP COLUMN host_actor_id,
  DROP COLUMN host_owner_id,
  DROP COLUMN host_id;
