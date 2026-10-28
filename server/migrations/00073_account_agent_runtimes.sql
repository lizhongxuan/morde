-- +goose Up
CREATE TABLE account_agent_runtimes (
  service_instance_id uuid NOT NULL REFERENCES service_identity(instance_id) ON DELETE RESTRICT,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  runtime_id text NOT NULL CHECK (runtime_id <> ''),
  runtime_generation bigint NOT NULL CHECK (runtime_generation > 0),
  state text NOT NULL CHECK (state IN ('starting', 'running', 'stopping', 'stopped', 'failed')),
  lease_owner text NOT NULL CHECK (lease_owner <> ''),
  lease_version bigint NOT NULL CHECK (lease_version > 0),
  execution_epoch bigint NOT NULL CHECK (execution_epoch > 0),
  lease_expires_at timestamptz,
  max_concurrent_tasks integer NOT NULL CHECK (max_concurrent_tasks BETWEEN 1 AND 16),
  runtime_sequence bigint NOT NULL DEFAULT 0 CHECK (runtime_sequence >= 0),
  control_version bigint NOT NULL DEFAULT 1 CHECK (control_version > 0),
  guardian_stop_attestation bytea,
  guardian_stop_attestation_digest bytea,
  stopped_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (service_instance_id, owner_id),
  UNIQUE (service_instance_id, runtime_id),
  CHECK (
    (guardian_stop_attestation IS NULL AND guardian_stop_attestation_digest IS NULL)
    OR (guardian_stop_attestation IS NOT NULL AND octet_length(guardian_stop_attestation) BETWEEN 1 AND 16384
        AND guardian_stop_attestation_digest IS NOT NULL AND octet_length(guardian_stop_attestation_digest) = 32)
  ),
  CHECK (
    (state IN ('starting', 'running', 'stopping') AND lease_expires_at IS NOT NULL AND stopped_at IS NULL
      AND guardian_stop_attestation IS NULL AND guardian_stop_attestation_digest IS NULL)
    OR (state = 'stopped' AND lease_expires_at IS NULL AND stopped_at IS NOT NULL
      AND guardian_stop_attestation IS NOT NULL AND guardian_stop_attestation_digest IS NOT NULL)
    OR state = 'failed'
  )
);

ALTER TABLE task_agent_instances
  ADD COLUMN service_instance_id uuid REFERENCES service_identity(instance_id) ON DELETE RESTRICT,
  ADD COLUMN account_runtime_id text,
  ADD COLUMN account_runtime_generation bigint,
  ADD COLUMN runtime_sequence bigint,
  ADD CONSTRAINT task_agent_runtime_binding_check CHECK (
    (service_instance_id IS NULL AND account_runtime_id IS NULL AND account_runtime_generation IS NULL AND runtime_sequence IS NULL)
    OR (service_instance_id IS NOT NULL AND account_runtime_id IS NOT NULL AND account_runtime_id <> ''
      AND account_runtime_generation IS NOT NULL AND account_runtime_generation > 0
      AND runtime_sequence IS NOT NULL AND runtime_sequence > 0)
  );

CREATE UNIQUE INDEX task_agent_runtime_sequence_unique
  ON task_agent_instances(service_instance_id, owner_id, account_runtime_id, account_runtime_generation, runtime_sequence)
  WHERE service_instance_id IS NOT NULL;

-- +goose Down
DROP INDEX task_agent_runtime_sequence_unique;
ALTER TABLE task_agent_instances
  DROP CONSTRAINT task_agent_runtime_binding_check,
  DROP COLUMN runtime_sequence,
  DROP COLUMN account_runtime_generation,
  DROP COLUMN account_runtime_id,
  DROP COLUMN service_instance_id;
DROP TABLE account_agent_runtimes;
