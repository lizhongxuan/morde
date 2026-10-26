-- +goose Up
CREATE UNIQUE INDEX tokens_owner_id_id ON tokens(owner_id, id);

CREATE TABLE mcp_runtime_sessions (
  owner_id            text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  api_key_id          text NOT NULL,
  session_id          text NOT NULL CHECK (length(session_id) BETWEEN 1 AND 255),
  host_id             text NOT NULL,
  runtime_task_id     text NOT NULL UNIQUE CHECK (runtime_task_id ~ '^q[a-z0-9]{7}$'),
  lease_version       bigint NOT NULL DEFAULT 0 CHECK (lease_version >= 0),
  lease_expires_at    timestamptz,
  active_operation_id text,
  created_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (owner_id, api_key_id, session_id, host_id),
  FOREIGN KEY (owner_id, api_key_id) REFERENCES tokens(owner_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (host_id, owner_id) REFERENCES hosts(id, owner_id) ON DELETE CASCADE,
  CHECK ((lease_version = 0 AND lease_expires_at IS NULL) OR (lease_version > 0 AND lease_expires_at IS NOT NULL))
);

CREATE TABLE mcp_runtime_operations (
  operation_id        text PRIMARY KEY CHECK (operation_id ~ '^o[a-z0-9]{7}$'),
  owner_id            text NOT NULL,
  api_key_id          text NOT NULL,
  session_id          text NOT NULL,
  host_id             text NOT NULL,
  idempotency_key     text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 255),
  request_hash        bytea NOT NULL CHECK (octet_length(request_hash) = 32),
  runtime_generation  bigint NOT NULL CHECK (runtime_generation > 0),
  runtime_instance_id text NOT NULL CHECK (length(runtime_instance_id) BETWEEN 1 AND 255),
  control_version     bigint NOT NULL CHECK (control_version >= 0),
  execution_epoch     bigint NOT NULL CHECK (execution_epoch > 0),
  lease_version       bigint NOT NULL CHECK (lease_version > 0),
  lease_expires_at    timestamptz NOT NULL,
  capability          text NOT NULL CHECK (length(capability) BETWEEN 1 AND 128),
  action              text NOT NULL CHECK (length(action) BETWEEN 1 AND 128),
  arguments           jsonb NOT NULL,
  status              text NOT NULL CHECK (status IN ('prepared', 'dispatched', 'succeeded', 'failed', 'cancelled', 'unknown')),
  result              jsonb NOT NULL DEFAULT 'null'::jsonb,
  cancel_requested_at timestamptz,
  created_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
  UNIQUE (owner_id, api_key_id, session_id, host_id, idempotency_key),
  UNIQUE (owner_id, api_key_id, session_id, host_id, operation_id),
  FOREIGN KEY (owner_id, api_key_id, session_id, host_id)
    REFERENCES mcp_runtime_sessions(owner_id, api_key_id, session_id, host_id) ON DELETE CASCADE
);

ALTER TABLE mcp_runtime_sessions
  ADD CONSTRAINT mcp_runtime_sessions_active_operation_fk
  FOREIGN KEY (owner_id, api_key_id, session_id, host_id, active_operation_id)
  REFERENCES mcp_runtime_operations(owner_id, api_key_id, session_id, host_id, operation_id)
  DEFERRABLE INITIALLY DEFERRED;

CREATE UNIQUE INDEX mcp_runtime_operations_one_active_per_session
  ON mcp_runtime_operations(owner_id, api_key_id, session_id, host_id)
  WHERE status IN ('prepared', 'dispatched', 'unknown');
CREATE INDEX mcp_runtime_operations_owner_active
  ON mcp_runtime_operations(owner_id, created_at, operation_id)
  WHERE status IN ('prepared', 'dispatched', 'unknown');
CREATE INDEX mcp_runtime_operations_pending_cancellation
  ON mcp_runtime_operations(owner_id, created_at, operation_id)
  WHERE cancel_requested_at IS NOT NULL AND status IN ('prepared', 'dispatched', 'unknown');

-- +goose Down
ALTER TABLE mcp_runtime_sessions DROP CONSTRAINT mcp_runtime_sessions_active_operation_fk;
DROP TABLE mcp_runtime_operations;
DROP TABLE mcp_runtime_sessions;
DROP INDEX tokens_owner_id_id;
