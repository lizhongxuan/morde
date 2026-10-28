-- +goose Up
ALTER TABLE account_agent_runtimes
  ADD COLUMN guardian_stop_acknowledged_at timestamptz,
  ADD CONSTRAINT account_agent_runtime_guardian_stop_ack_check CHECK (
    guardian_stop_acknowledged_at IS NULL OR guardian_stop_attestation IS NOT NULL
  );

ALTER TABLE task_agent_instances
  ADD COLUMN guardian_task_stop_ack_required boolean NOT NULL DEFAULT false,
  ADD COLUMN guardian_stop_acknowledged_at timestamptz;

UPDATE task_agent_instances
SET guardian_task_stop_ack_required=true
WHERE service_instance_id IS NOT NULL AND guardian_stop_attestation IS NOT NULL
  AND convert_from(guardian_stop_attestation,'UTF8')::jsonb ? 'task_fence';

ALTER TABLE task_agent_instances
  ADD CONSTRAINT task_agent_guardian_stop_ack_check CHECK (
    (NOT guardian_task_stop_ack_required AND guardian_stop_acknowledged_at IS NULL)
    OR (guardian_task_stop_ack_required AND service_instance_id IS NOT NULL AND guardian_stop_attestation IS NOT NULL)
  );

CREATE INDEX task_agent_guardian_stop_ack_pending
  ON task_agent_instances(service_instance_id, stopped_at, task_id, agent_generation)
  WHERE guardian_task_stop_ack_required
    AND guardian_stop_acknowledged_at IS NULL;

CREATE INDEX account_agent_runtime_guardian_stop_ack_pending
  ON account_agent_runtimes(service_instance_id, stopped_at, owner_id)
  WHERE state='stopped' AND guardian_stop_attestation IS NOT NULL
    AND guardian_stop_acknowledged_at IS NULL;

-- +goose Down
DROP INDEX account_agent_runtime_guardian_stop_ack_pending;
DROP INDEX task_agent_guardian_stop_ack_pending;
ALTER TABLE task_agent_instances
  DROP CONSTRAINT task_agent_guardian_stop_ack_check,
  DROP COLUMN guardian_stop_acknowledged_at,
  DROP COLUMN guardian_task_stop_ack_required;
ALTER TABLE account_agent_runtimes
  DROP CONSTRAINT account_agent_runtime_guardian_stop_ack_check,
  DROP COLUMN guardian_stop_acknowledged_at;
