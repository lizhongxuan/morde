-- +goose Up
ALTER TABLE task_workspace_publish_receipts
  DROP CONSTRAINT task_workspace_publish_receipts_state_check,
  ADD CONSTRAINT task_workspace_publish_receipts_state_check
    CHECK (state IN ('pending', 'succeeded', 'conflict', 'quota_rejected', 'superseded'));

ALTER TABLE task_agent_message_deliveries
  ADD COLUMN claimed_checkpoint_through_seq bigint CHECK (claimed_checkpoint_through_seq >= 0);

UPDATE task_agent_message_deliveries AS delivery
SET claimed_checkpoint_through_seq = task.checkpoint_through_seq
FROM tasks AS task
WHERE delivery.task_id=task.id AND delivery.owner_id=task.owner_id
  AND delivery.session_id=task.agent_session_id AND delivery.claimed_through_seq IS NOT NULL;

-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (
    SELECT 1 FROM task_agent_message_deliveries delivery
    WHERE delivery.claimed_through_seq IS NOT NULL AND delivery.claimed_checkpoint_through_seq IS NULL
  ) THEN
    RAISE EXCEPTION 'cannot backfill claimed Task message without its root checkpoint';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE task_agent_sessions
  ADD COLUMN root_checkpoint_through_seq bigint NOT NULL DEFAULT -1 CHECK (root_checkpoint_through_seq >= -1),
  ADD COLUMN root_checkpoint_baseline_known boolean NOT NULL DEFAULT true;

-- Existing children cannot be safely mapped from their local DSH sequence to
-- the root session's sequence. Preserve the unknown state and fail closed for
-- their post-steer tool calls; newly created roots are known by default.
UPDATE task_agent_sessions
SET root_checkpoint_baseline_known=false
WHERE parent_session_id IS NOT NULL;

UPDATE task_agent_sessions AS root
SET root_checkpoint_through_seq = task.checkpoint_through_seq
FROM tasks AS task
WHERE task.id=root.task_id AND task.owner_id=root.owner_id AND root.parent_session_id IS NULL;

CREATE TABLE task_agent_tool_call_admissions (
  task_id            text NOT NULL,
  owner_id           text NOT NULL,
  operation_key      text NOT NULL CHECK (length(operation_key) BETWEEN 1 AND 512),
  agent_instance_id  text NOT NULL,
  agent_generation   bigint NOT NULL CHECK (agent_generation > 0),
  root_session_id    text NOT NULL,
  call_session_id    text NOT NULL,
  through_sequence   bigint NOT NULL CHECK (through_sequence >= 0),
  root_through_sequence bigint NOT NULL CHECK (root_through_sequence >= -1),
  created_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (task_id,operation_key),
  FOREIGN KEY (task_id,owner_id) REFERENCES tasks(id,owner_id) ON DELETE CASCADE
);

CREATE TABLE task_agent_tool_supersessions (
  task_id       text NOT NULL,
  owner_id      text NOT NULL,
  operation_key text NOT NULL CHECK (length(operation_key) BETWEEN 1 AND 512),
  message_id    text NOT NULL REFERENCES conversation_messages(id) ON DELETE RESTRICT,
  position      integer NOT NULL CHECK (position >= 0),
  created_at    timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (task_id, operation_key, message_id),
  UNIQUE (task_id, operation_key, position),
  FOREIGN KEY (task_id, owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE
);

CREATE INDEX task_agent_tool_supersessions_message_idx
  ON task_agent_tool_supersessions(message_id);

CREATE TABLE task_agent_child_cancel_operations (
  task_id       text NOT NULL,
  owner_id      text NOT NULL,
  operation_key text NOT NULL CHECK (length(operation_key) BETWEEN 1 AND 512),
  session_id    text NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (task_id, operation_key),
  FOREIGN KEY (task_id, owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE,
  FOREIGN KEY (task_id, session_id) REFERENCES task_agent_sessions(task_id, session_id) ON DELETE CASCADE
);

CREATE INDEX task_agent_child_cancel_operations_session_idx
  ON task_agent_child_cancel_operations(task_id, session_id);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM task_agent_tool_supersessions)
     OR EXISTS (SELECT 1 FROM task_agent_child_cancel_operations)
     OR EXISTS (SELECT 1 FROM task_agent_tool_call_admissions)
     OR EXISTS (SELECT 1 FROM task_agent_message_deliveries WHERE claimed_checkpoint_through_seq IS NOT NULL)
     OR EXISTS (SELECT 1 FROM task_agent_sessions WHERE parent_session_id IS NOT NULL AND root_checkpoint_baseline_known)
     OR EXISTS (SELECT 1 FROM task_workspace_publish_receipts WHERE state='superseded') THEN
    RAISE EXCEPTION 'cannot remove persisted Task tool supersession evidence';
  END IF;
END $$;
-- +goose StatementEnd

DROP INDEX task_agent_tool_supersessions_message_idx;
DROP INDEX task_agent_child_cancel_operations_session_idx;
DROP TABLE task_agent_child_cancel_operations;
DROP TABLE task_agent_tool_call_admissions;
DROP TABLE task_agent_tool_supersessions;

ALTER TABLE task_agent_sessions DROP COLUMN root_checkpoint_baseline_known, DROP COLUMN root_checkpoint_through_seq;

ALTER TABLE task_agent_message_deliveries DROP COLUMN claimed_checkpoint_through_seq;

ALTER TABLE task_workspace_publish_receipts
  DROP CONSTRAINT task_workspace_publish_receipts_state_check,
  ADD CONSTRAINT task_workspace_publish_receipts_state_check
    CHECK (state IN ('pending', 'succeeded', 'conflict', 'quota_rejected'));
