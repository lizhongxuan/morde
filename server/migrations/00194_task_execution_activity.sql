-- +goose Up
ALTER TABLE task_steps
  ADD COLUMN agent_session_id text,
  ADD COLUMN task_revision bigint,
  ADD COLUMN tool_call_key text,
  ADD COLUMN tool_name text,
  ADD COLUMN request_digest bytea,
  ADD COLUMN phase text,
  ADD COLUMN detail_state text NOT NULL DEFAULT 'not_recorded' CHECK (detail_state IN ('not_recorded', 'available', 'expired'));

ALTER TABLE task_steps
  ADD CONSTRAINT task_steps_revision_check CHECK (task_revision IS NULL OR task_revision >= 0),
  ADD CONSTRAINT task_steps_request_digest_check CHECK (request_digest IS NULL OR octet_length(request_digest) = 32),
  ADD CONSTRAINT task_steps_tool_call_identity_check CHECK (
    (agent_session_id IS NULL AND task_revision IS NULL AND tool_call_key IS NULL AND tool_name IS NULL AND request_digest IS NULL)
    OR (agent_session_id IS NOT NULL AND task_revision IS NOT NULL AND tool_call_key IS NOT NULL AND tool_name IS NOT NULL AND request_digest IS NOT NULL)
  ),
  ADD CONSTRAINT task_steps_agent_session_fk FOREIGN KEY (task_id, agent_session_id)
    REFERENCES task_agent_sessions(task_id, session_id) ON DELETE CASCADE;

CREATE UNIQUE INDEX task_steps_tool_call_identity
  ON task_steps(task_id, agent_session_id, tool_call_key)
  WHERE tool_call_key IS NOT NULL;

ALTER TABLE task_agent_sessions
  ADD COLUMN activity_status text,
  ADD COLUMN activity_generation bigint,
  ADD COLUMN activity_sequence bigint,
  ADD COLUMN activity_observed_at timestamptz,
  ADD CONSTRAINT task_agent_sessions_activity_check CHECK (
    (activity_status IS NULL AND activity_generation IS NULL AND activity_sequence IS NULL AND activity_observed_at IS NULL)
    OR (activity_status IS NOT NULL AND activity_status IN ('idle', 'running') AND activity_generation IS NOT NULL AND activity_generation > 0
      AND activity_sequence IS NOT NULL AND activity_sequence > 0 AND activity_observed_at IS NOT NULL)
  );

CREATE TABLE task_step_details (
  task_id             text NOT NULL,
  owner_id            text NOT NULL,
  step_id             text NOT NULL,
  task_revision       bigint NOT NULL CHECK (task_revision >= 0),
  payload_ciphertext  bytea NOT NULL CHECK (octet_length(payload_ciphertext) BETWEEN 29 AND 65564),
  payload_sha256      bytea NOT NULL CHECK (octet_length(payload_sha256) = 32),
  payload_size_bytes  integer NOT NULL CHECK (payload_size_bytes BETWEEN 1 AND 65536),
  truncated           boolean NOT NULL DEFAULT false,
  created_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (task_id, step_id),
  FOREIGN KEY (task_id, owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE,
  FOREIGN KEY (task_id, step_id) REFERENCES task_steps(task_id, id) ON DELETE CASCADE
);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM task_step_details) THEN
    RAISE EXCEPTION 'cannot roll back while encrypted Task step details exist';
  END IF;
  IF EXISTS (SELECT 1 FROM task_steps WHERE tool_call_key IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot roll back while Task step call identities exist';
  END IF;
  IF EXISTS (SELECT 1 FROM task_agent_sessions WHERE activity_status IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot roll back while Task agent activity exists';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE task_step_details;
DROP INDEX task_steps_tool_call_identity;
ALTER TABLE task_steps DROP CONSTRAINT task_steps_agent_session_fk,
  DROP CONSTRAINT task_steps_tool_call_identity_check,
  DROP CONSTRAINT task_steps_request_digest_check,
  DROP CONSTRAINT task_steps_revision_check,
  DROP COLUMN detail_state,
  DROP COLUMN phase,
  DROP COLUMN request_digest,
  DROP COLUMN tool_name,
  DROP COLUMN tool_call_key,
  DROP COLUMN task_revision,
  DROP COLUMN agent_session_id;
ALTER TABLE task_agent_sessions DROP CONSTRAINT task_agent_sessions_activity_check,
  DROP COLUMN activity_observed_at,
  DROP COLUMN activity_sequence,
  DROP COLUMN activity_generation,
  DROP COLUMN activity_status;
