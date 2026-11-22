-- +goose Up
ALTER TABLE task_agent_sessions
  ADD COLUMN activity_revision bigint,
  ADD COLUMN activity_phase text,
  ADD COLUMN activity_state text,
  ADD COLUMN activity_id text,
  ADD COLUMN activity_progressed_at timestamptz,
  ADD COLUMN activity_heartbeat_at timestamptz,
  ADD CONSTRAINT task_agent_sessions_activity_revision_check CHECK (activity_revision IS NULL OR activity_revision >= 0),
  ADD CONSTRAINT task_agent_sessions_activity_phase_check CHECK (activity_phase IS NULL OR activity_phase IN ('model', 'tool', 'subagent', 'checkpoint')),
  ADD CONSTRAINT task_agent_sessions_activity_state_check CHECK (activity_state IS NULL OR activity_state IN ('started', 'waiting', 'completed', 'failed', 'cancelled')),
  ADD CONSTRAINT task_agent_sessions_activity_identity_check CHECK (
    (activity_phase IS NULL AND activity_state IS NULL AND activity_id IS NULL)
    OR (activity_phase IS NOT NULL AND activity_state IS NOT NULL AND activity_id IS NOT NULL)
  ),
  ADD CONSTRAINT task_agent_sessions_activity_id_check CHECK (activity_id IS NULL OR octet_length(activity_id) BETWEEN 1 AND 256);

CREATE TABLE task_agent_stall_notices (
  task_id       text NOT NULL,
  owner_id      text NOT NULL,
  message_id    text NOT NULL REFERENCES conversation_messages(id) ON DELETE RESTRICT,
  step_id       text NOT NULL,
  task_revision bigint NOT NULL CHECK (task_revision >= 0),
  agent_generation bigint NOT NULL CHECK (agent_generation > 0),
  state         text NOT NULL CHECK (state IN ('active', 'cleared')),
  prompted_at   timestamptz NOT NULL DEFAULT clock_timestamp(),
  cleared_at    timestamptz,
  PRIMARY KEY (task_id, message_id),
  UNIQUE (task_id, step_id),
  FOREIGN KEY (task_id, owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE,
  FOREIGN KEY (task_id, step_id) REFERENCES task_steps(task_id, id) ON DELETE CASCADE,
  CHECK ((state = 'active' AND cleared_at IS NULL) OR (state = 'cleared' AND cleared_at IS NOT NULL))
);

CREATE INDEX task_agent_stall_notices_active
  ON task_agent_stall_notices(task_id, owner_id) WHERE state = 'active';

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM task_agent_stall_notices)
     OR EXISTS (SELECT 1 FROM task_agent_sessions WHERE activity_phase IS NOT NULL OR activity_heartbeat_at IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot roll back while Task agent liveness evidence exists';
  END IF;
END $$;
-- +goose StatementEnd

DROP INDEX task_agent_stall_notices_active;
DROP TABLE task_agent_stall_notices;
ALTER TABLE task_agent_sessions
  DROP CONSTRAINT task_agent_sessions_activity_id_check,
  DROP CONSTRAINT task_agent_sessions_activity_identity_check,
  DROP CONSTRAINT task_agent_sessions_activity_state_check,
  DROP CONSTRAINT task_agent_sessions_activity_phase_check,
  DROP CONSTRAINT task_agent_sessions_activity_revision_check,
  DROP COLUMN activity_heartbeat_at,
  DROP COLUMN activity_progressed_at,
  DROP COLUMN activity_id,
  DROP COLUMN activity_state,
  DROP COLUMN activity_phase,
  DROP COLUMN activity_revision;
