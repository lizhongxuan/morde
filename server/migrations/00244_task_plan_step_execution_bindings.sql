-- +goose Up
CREATE TABLE task_plan_step_execution_bindings (
  owner_id             text NOT NULL,
  task_id              text NOT NULL,
  task_revision        bigint NOT NULL CHECK (task_revision >= 0),
  plan_revision        bigint NOT NULL CHECK (plan_revision > 0),
  plan_step_id         text NOT NULL CHECK (plan_step_id ~ '^[A-Za-z0-9][A-Za-z0-9_-]{0,39}$'),
  execution_kind       text NOT NULL CHECK (execution_kind IN ('operation', 'mcp_connector', 'child_session')),
  execution_id         text NOT NULL CHECK (char_length(execution_id) BETWEEN 1 AND 256),
  agent_session_id     text NOT NULL CHECK (char_length(agent_session_id) BETWEEN 1 AND 128),
  call_id              text NOT NULL CHECK (char_length(call_id) BETWEEN 1 AND 256),
  tool_name            text NOT NULL DEFAULT '',
  connector_id         text NOT NULL DEFAULT '',
  child_step_id        text NOT NULL DEFAULT '',
  parent_step_id       text NOT NULL DEFAULT '',
  request_digest       bytea NOT NULL CHECK (octet_length(request_digest) = 32),
  authorization_version bigint NOT NULL CHECK (authorization_version > 0),
  created_at           timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (owner_id, task_id, task_revision, plan_revision, execution_kind, execution_id),
  FOREIGN KEY (task_id, owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE,
  CHECK (
    (execution_kind = 'operation' AND tool_name <> '' AND connector_id = '' AND
      child_step_id = '' AND parent_step_id = '') OR
    (execution_kind = 'mcp_connector' AND tool_name <> '' AND connector_id <> '' AND
      child_step_id = '' AND parent_step_id = '') OR
    (execution_kind = 'child_session' AND tool_name = '' AND connector_id = '' AND
      child_step_id <> '' AND execution_id <> agent_session_id)
  )
);

CREATE INDEX task_plan_step_execution_bindings_step_idx
  ON task_plan_step_execution_bindings
  (owner_id, task_id, task_revision, plan_revision, plan_step_id, execution_kind, execution_id);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM task_plan_step_execution_bindings) THEN
    RAISE EXCEPTION 'cannot discard task plan step execution bindings';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE task_plan_step_execution_bindings;
