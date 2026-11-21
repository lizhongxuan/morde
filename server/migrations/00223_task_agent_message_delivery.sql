-- +goose Up
ALTER TABLE conversation_messages
  ADD COLUMN delivery_mode text NOT NULL DEFAULT 'followup'
  CHECK (delivery_mode IN ('steer', 'followup'));

CREATE TABLE task_agent_message_deliveries (
  message_id text PRIMARY KEY REFERENCES conversation_messages(id) ON DELETE RESTRICT,
  task_id text NOT NULL REFERENCES tasks(id) ON DELETE RESTRICT,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  initiator_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  agent_instance_id text NOT NULL,
  agent_generation bigint NOT NULL CHECK (agent_generation > 0),
  authorization_version bigint NOT NULL CHECK (authorization_version > 0),
  session_id text NOT NULL,
  mode text NOT NULL CHECK (mode IN ('steer', 'followup')),
  enqueued_through_seq bigint CHECK (enqueued_through_seq >= 0),
  claimed_through_seq bigint CHECK (claimed_through_seq >= 0),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  CHECK (claimed_through_seq IS NULL OR enqueued_through_seq IS NULL OR claimed_through_seq > enqueued_through_seq)
);

CREATE INDEX task_agent_message_deliveries_task
  ON task_agent_message_deliveries(task_id, owner_id, message_id);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM task_agent_message_deliveries)
     OR EXISTS (SELECT 1 FROM conversation_messages WHERE delivery_mode <> 'followup') THEN
    RAISE EXCEPTION 'cannot remove persisted task message delivery evidence';
  END IF;
END $$;
-- +goose StatementEnd

DROP INDEX task_agent_message_deliveries_task;
DROP TABLE task_agent_message_deliveries;
ALTER TABLE conversation_messages DROP COLUMN delivery_mode;
