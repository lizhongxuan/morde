-- +goose Up
-- A message waiting for causal evidence must not monopolize the route queue.
ALTER TABLE conversation_message_routes
  ADD COLUMN available_after timestamptz NOT NULL DEFAULT clock_timestamp();

ALTER TABLE conversation_message_routes DROP CONSTRAINT conversation_message_routes_state_check;
ALTER TABLE conversation_message_routes ADD CONSTRAINT conversation_message_routes_state_check
  CHECK (state IN ('pending', 'processing', 'routed', 'waiting_user', 'waiting_adjustment', 'failed'));

-- Modification intents do not enter an active Agent inbox. A later revision
-- coordinator must prove the old execution stopped before applying them.
CREATE TABLE task_revision_intents (
  message_id text PRIMARY KEY REFERENCES conversation_message_routes(message_id) ON DELETE RESTRICT,
  task_id text NOT NULL REFERENCES tasks(id) ON DELETE RESTRICT,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  initiator_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  conversation_id text NOT NULL,
  message_sequence bigint NOT NULL CHECK (message_sequence > 0),
  expected_revision bigint NOT NULL CHECK (expected_revision >= 0),
  state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'stopping', 'applied', 'failed')),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  FOREIGN KEY (conversation_id, owner_id) REFERENCES conversations(id, owner_id) ON DELETE RESTRICT
);

CREATE INDEX task_revision_intents_pending
  ON task_revision_intents(task_id, message_sequence, message_id)
  WHERE state IN ('pending', 'stopping');

CREATE INDEX conversation_message_routes_available
  ON conversation_message_routes(available_after, created_at, message_id)
  WHERE state IN ('pending', 'processing');

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM conversation_message_routes WHERE state IN ('pending','processing','waiting_adjustment'))
     OR EXISTS (SELECT 1 FROM task_revision_intents) THEN
    RAISE EXCEPTION 'cannot remove route backoff while messages await routing';
  END IF;
END $$;
-- +goose StatementEnd

DROP INDEX conversation_message_routes_available;
DROP INDEX task_revision_intents_pending;
DROP TABLE task_revision_intents;
ALTER TABLE conversation_message_routes DROP CONSTRAINT conversation_message_routes_state_check;
ALTER TABLE conversation_message_routes ADD CONSTRAINT conversation_message_routes_state_check
  CHECK (state IN ('pending', 'processing', 'routed', 'waiting_user', 'failed'));
ALTER TABLE conversation_message_routes DROP COLUMN available_after;
