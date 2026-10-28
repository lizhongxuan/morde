-- +goose Up
CREATE TABLE task_agent_sessions (
  task_id text NOT NULL,
  owner_id text NOT NULL,
  session_id text NOT NULL CHECK (session_id <> ''),
  parent_session_id text,
  step_id text,
  spawn_key text,
  request_hash bytea,
  checkpoint_through_seq bigint NOT NULL DEFAULT -1 CHECK (checkpoint_through_seq >= -1),
  checkpoint_digest bytea CHECK (checkpoint_digest IS NULL OR octet_length(checkpoint_digest) = 32),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (task_id, session_id),
  FOREIGN KEY (task_id, owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE,
  FOREIGN KEY (task_id, parent_session_id) REFERENCES task_agent_sessions(task_id, session_id) ON DELETE CASCADE,
  FOREIGN KEY (task_id, step_id) REFERENCES task_steps(task_id, id) ON DELETE CASCADE,
  CHECK (
    (parent_session_id IS NULL AND step_id IS NULL AND spawn_key IS NULL AND request_hash IS NULL)
    OR (parent_session_id IS NOT NULL AND parent_session_id <> session_id AND step_id IS NOT NULL
      AND spawn_key IS NOT NULL AND spawn_key <> '' AND request_hash IS NOT NULL AND octet_length(request_hash) = 32)
  ),
  CHECK (
    (checkpoint_through_seq = -1 AND checkpoint_digest IS NULL)
    OR (checkpoint_through_seq >= 0 AND checkpoint_digest IS NOT NULL)
  )
);

CREATE UNIQUE INDEX task_agent_sessions_one_root
  ON task_agent_sessions(task_id) WHERE parent_session_id IS NULL;
CREATE UNIQUE INDEX task_agent_sessions_spawn_key
  ON task_agent_sessions(task_id, parent_session_id, spawn_key) WHERE parent_session_id IS NOT NULL;
CREATE UNIQUE INDEX task_agent_sessions_step
  ON task_agent_sessions(task_id, step_id) WHERE step_id IS NOT NULL;

INSERT INTO task_agent_sessions (task_id, owner_id, session_id, checkpoint_through_seq, checkpoint_digest)
SELECT id, owner_id, agent_session_id, checkpoint_through_seq, checkpoint_digest
FROM tasks
WHERE agent_session_id <> '';

ALTER TABLE task_agent_checkpoints
  ADD CONSTRAINT task_agent_checkpoints_session_fkey
  FOREIGN KEY (task_id, agent_session_id) REFERENCES task_agent_sessions(task_id, session_id) ON DELETE CASCADE;

ALTER TABLE task_agent_checkpoints
  ADD COLUMN agent_root_session_id text;
UPDATE task_agent_checkpoints c
SET agent_root_session_id=t.agent_session_id
FROM tasks t
WHERE t.id=c.task_id AND t.owner_id=c.owner_id;
ALTER TABLE task_agent_checkpoints
  ALTER COLUMN agent_root_session_id SET NOT NULL,
  ADD CONSTRAINT task_agent_checkpoints_root_session_fkey
  FOREIGN KEY (task_id, agent_root_session_id) REFERENCES task_agent_sessions(task_id, session_id) ON DELETE CASCADE;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM task_agent_sessions WHERE parent_session_id IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot roll back while child Agent Sessions exist';
  END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE task_agent_checkpoints
  DROP CONSTRAINT task_agent_checkpoints_root_session_fkey,
  DROP CONSTRAINT task_agent_checkpoints_session_fkey,
  DROP COLUMN agent_root_session_id;
DROP TABLE task_agent_sessions;
