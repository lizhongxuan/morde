-- +goose Up
ALTER TABLE task_sandbox_call_receipts
  ADD COLUMN agent_instance_id text NOT NULL DEFAULT '' CHECK (length(agent_instance_id) <= 128),
  ADD COLUMN agent_generation bigint NOT NULL DEFAULT 0 CHECK (agent_generation >= 0),
  ADD COLUMN agent_session_id text NOT NULL DEFAULT '' CHECK (length(agent_session_id) <= 128),
  ADD COLUMN agent_engine_version text NOT NULL DEFAULT '' CHECK (length(agent_engine_version) <= 128),
  ADD COLUMN agent_profile_version text NOT NULL DEFAULT '' CHECK (length(agent_profile_version) <= 128),
  ADD CONSTRAINT task_sandbox_call_agent_fence_check CHECK (
    (agent_instance_id = '' AND agent_generation = 0 AND agent_session_id = '' AND agent_engine_version = '' AND agent_profile_version = '')
    OR
    (agent_instance_id <> '' AND agent_generation > 0 AND agent_session_id <> '' AND agent_engine_version <> '' AND agent_profile_version <> '')
  );

-- +goose Down
ALTER TABLE task_sandbox_call_receipts
  DROP CONSTRAINT task_sandbox_call_agent_fence_check,
  DROP COLUMN agent_instance_id,
  DROP COLUMN agent_generation,
  DROP COLUMN agent_session_id,
  DROP COLUMN agent_engine_version,
  DROP COLUMN agent_profile_version;
