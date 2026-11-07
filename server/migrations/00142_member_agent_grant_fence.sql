-- +goose Up
-- Do not silently adopt a live/retained member Agent into whatever grant is
-- current at migration time. Its original grant snapshot is unknowable.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM task_agent_instances WHERE host_authorization_kind = 'member') THEN
    RAISE EXCEPTION 'cannot add member Agent grant fences while legacy member Agent records exist';
  END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE task_agent_instances
  ADD COLUMN host_grant_fingerprint text;

ALTER TABLE task_agent_instances
  DROP CONSTRAINT task_agent_host_fence_check,
  ADD CONSTRAINT task_agent_host_fence_check CHECK (
    (
      host_id IS NULL AND host_owner_id IS NULL AND host_actor_id IS NULL AND
      host_permission IS NULL AND host_authorization_kind IS NULL AND
      host_runtime_generation IS NULL AND host_runtime_instance_id IS NULL AND
      host_control_version IS NULL AND host_grant_fingerprint IS NULL
    ) OR (
      host_id IS NOT NULL AND length(host_id) BETWEEN 1 AND 128 AND
      host_owner_id IS NOT NULL AND host_owner_id <> '' AND
      host_actor_id IS NOT NULL AND host_actor_id <> '' AND
      host_permission = 'desktop:control' AND
      host_authorization_kind IN ('owner', 'member') AND
      host_runtime_generation IS NOT NULL AND host_runtime_generation > 0 AND
      host_runtime_instance_id IS NOT NULL AND length(host_runtime_instance_id) BETWEEN 1 AND 128 AND
      host_control_version IS NOT NULL AND host_control_version >= 0 AND
      (
        (host_authorization_kind = 'owner' AND host_actor_id = host_owner_id AND host_grant_fingerprint IS NULL) OR
        (host_authorization_kind = 'member' AND host_actor_id <> host_owner_id AND host_grant_fingerprint ~ '^[0-9a-f]{64}$')
      )
    )
  );

-- +goose Down
-- Preserve the durable grant fence whenever a member Agent instance still
-- exists. Removing the fingerprint would let its renewal snapshot be lost.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM task_agent_instances WHERE host_authorization_kind = 'member'
  ) THEN
    RAISE EXCEPTION 'cannot roll back member Agent grant fences while member Agent records exist';
  END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE task_agent_instances
  DROP CONSTRAINT task_agent_host_fence_check,
  DROP COLUMN host_grant_fingerprint,
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
  );
