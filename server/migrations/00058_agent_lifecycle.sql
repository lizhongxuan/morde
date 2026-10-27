-- +goose Up
CREATE TABLE task_agent_instances (
  task_id text NOT NULL,
  owner_id text NOT NULL,
  agent_instance_id text NOT NULL CHECK (agent_instance_id <> ''),
  agent_generation bigint NOT NULL CHECK (agent_generation > 0),
  agent_session_id text NOT NULL CHECK (agent_session_id <> ''),
  engine_version text NOT NULL CHECK (engine_version <> ''),
  profile_version text NOT NULL CHECK (profile_version <> ''),
  supervisor_id text NOT NULL CHECK (supervisor_id <> ''),
  stop_nonce_digest bytea NOT NULL CHECK (octet_length(stop_nonce_digest) = 32),
  lease_owner text NOT NULL CHECK (lease_owner <> ''),
  lease_version bigint NOT NULL CHECK (lease_version > 0),
  auth_version bigint NOT NULL CHECK (auth_version > 0),
  initial_lease_ms bigint NOT NULL CHECK (initial_lease_ms BETWEEN 1 AND 30000),
  lease_expires_at timestamptz NOT NULL,
  state text NOT NULL CHECK (state IN ('active', 'stopped')),
  stop_receipt bytea,
  stop_receipt_digest bytea,
  stopped_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (task_id, agent_generation),
  UNIQUE (agent_instance_id),
  UNIQUE (supervisor_id, stop_nonce_digest),
  FOREIGN KEY (task_id, owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE,
  CHECK (
    (state = 'active' AND stop_receipt IS NULL AND stop_receipt_digest IS NULL AND stopped_at IS NULL)
    OR (state = 'stopped' AND stop_receipt IS NOT NULL AND octet_length(stop_receipt) BETWEEN 1 AND 4096
        AND stop_receipt_digest IS NOT NULL AND octet_length(stop_receipt_digest) = 32 AND stopped_at IS NOT NULL)
  )
);
CREATE UNIQUE INDEX task_agent_one_active ON task_agent_instances(task_id) WHERE state = 'active';

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM task_agent_instances) THEN
    RAISE EXCEPTION 'cannot roll back agent lifecycle while agent records exist';
  END IF;
END;
$$;
-- +goose StatementEnd
DROP TABLE task_agent_instances;
