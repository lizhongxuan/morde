-- +goose Up
ALTER TABLE task_agent_stall_notices
  ADD COLUMN notice_kind text NOT NULL DEFAULT 'activity_stall'
    CHECK (notice_kind IN ('activity_stall', 'request_reconciliation'));

CREATE TABLE task_agent_request_reconciliations (
  task_id               text NOT NULL,
  owner_id              text NOT NULL,
  session_id            text NOT NULL,
  task_revision         bigint NOT NULL CHECK (task_revision >= 0),
  agent_instance_id     text NOT NULL,
  agent_generation      bigint NOT NULL CHECK (agent_generation > 0),
  authorization_version bigint NOT NULL CHECK (authorization_version > 0),
  phase                 text NOT NULL CHECK (phase IN ('tool', 'subagent', 'checkpoint')),
  request_kind          text NOT NULL CHECK (request_kind IN ('tool_call', 'child_create', 'child_settle', 'checkpoint_write', 'checkpoint_read')),
  activity_id           text NOT NULL CHECK (octet_length(activity_id) BETWEEN 1 AND 256),
  request_digest        text NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
  observation_sequence  bigint NOT NULL CHECK (observation_sequence BETWEEN 1 AND 9007199254740991),
  state                 text NOT NULL CHECK (state IN ('pending', 'resolved')),
  recorded_at           timestamptz NOT NULL DEFAULT clock_timestamp(),
  resolved_at           timestamptz,
  PRIMARY KEY (task_id, session_id, task_revision, request_kind, activity_id, request_digest),
  FOREIGN KEY (task_id, owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE,
  FOREIGN KEY (task_id, session_id) REFERENCES task_agent_sessions(task_id, session_id) ON DELETE CASCADE,
  CHECK ((state = 'pending' AND resolved_at IS NULL) OR (state = 'resolved' AND resolved_at IS NOT NULL)),
  CHECK (
    (phase = 'tool' AND request_kind = 'tool_call') OR
    (phase = 'subagent' AND request_kind IN ('child_create', 'child_settle')) OR
    (phase = 'checkpoint' AND request_kind IN ('checkpoint_write', 'checkpoint_read'))
  )
);

CREATE INDEX task_agent_request_reconciliations_pending_revision
  ON task_agent_request_reconciliations(task_id, owner_id, task_revision)
  WHERE state = 'pending';

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM task_agent_request_reconciliations WHERE state = 'pending')
     OR EXISTS (SELECT 1 FROM task_agent_stall_notices WHERE notice_kind = 'request_reconciliation') THEN
    RAISE EXCEPTION 'cannot roll back while Task agent request reconciliation is pending';
  END IF;
END $$;
-- +goose StatementEnd

DROP INDEX task_agent_request_reconciliations_pending_revision;
DROP TABLE task_agent_request_reconciliations;
ALTER TABLE task_agent_stall_notices DROP COLUMN notice_kind;
