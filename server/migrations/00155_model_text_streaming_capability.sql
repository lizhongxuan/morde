-- +goose Up
-- A positive stream proof is valid only for the exact saved connection
-- version and explicit probe attempt that observed a complete metered SSE.
CREATE TABLE model_text_streaming_capabilities (
  model_id text PRIMARY KEY REFERENCES model_connections(id) ON DELETE CASCADE,
  connection_version timestamptz NOT NULL,
  actor_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  attempt_id text NOT NULL CHECK (attempt_id ~ '^probe_[a-f0-9]{64}$'),
  state text NOT NULL CHECK (state IN ('checking','verified')),
  verified_at timestamptz,
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  CHECK ((state='verified') = (verified_at IS NOT NULL))
);

-- Exact evidence of a streamed Task call, written in the same transaction as
-- dispatch. Deltas can only be persisted while the ledger, connection version
-- and full Task/Agent fence still match this record.
CREATE TABLE model_text_streaming_dispatches (
  owner_id text NOT NULL,
  source text NOT NULL DEFAULT 'task' CHECK (source='task'),
  call_id text NOT NULL,
  task_id text NOT NULL REFERENCES tasks(id) ON DELETE RESTRICT,
  task_owner_id text NOT NULL,
  initiator_id text NOT NULL CHECK (initiator_id=owner_id),
  task_revision bigint NOT NULL CHECK (task_revision>=0),
  model_id text NOT NULL,
  connection_version timestamptz NOT NULL,
  agent_instance_id text NOT NULL,
  agent_generation bigint NOT NULL CHECK (agent_generation>0),
  agent_session_id text NOT NULL,
  agent_root_session_id text NOT NULL,
  lease_owner text NOT NULL,
  lease_version bigint NOT NULL CHECK (lease_version>0),
  authorization_version bigint NOT NULL CHECK (authorization_version>0),
  through_sequence bigint NOT NULL CHECK (through_sequence>=0),
  engine_version text NOT NULL,
  profile_version text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (owner_id,source,call_id),
  FOREIGN KEY (owner_id,source,call_id) REFERENCES model_call_ledger(owner_id,source,call_id) ON DELETE CASCADE
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM model_text_streaming_dispatches) THEN
    RAISE EXCEPTION 'cannot remove dispatched stream evidence while model call records exist';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE model_text_streaming_dispatches;
DROP TABLE model_text_streaming_capabilities;
