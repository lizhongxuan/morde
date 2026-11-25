-- +goose Up
ALTER TABLE task_revision_intents
  ADD CONSTRAINT task_revision_intents_message_revision_unique
  UNIQUE (message_id, task_id, expected_revision);

ALTER TABLE task_clarification_requests
  ADD CONSTRAINT task_clarification_requests_id_task_unique
  UNIQUE (id, task_id);

CREATE TABLE conversation_route_file_delivery_clarifications (
  message_id text PRIMARY KEY,
  task_id text NOT NULL,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  initiator_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  conversation_id text NOT NULL,
  expected_revision bigint NOT NULL CHECK (expected_revision >= 0),
  clarification_id text NOT NULL UNIQUE,
  clarification_version bigint NOT NULL CHECK (clarification_version > 0),
  status text NOT NULL CHECK (status IN ('pending', 'answered', 'cancelled')),
  file_delivery_intent text CHECK (file_delivery_intent IN ('none', 'required')),
  answer text NOT NULL DEFAULT '' CHECK (char_length(answer) <= 16384),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  responded_at timestamptz,
  FOREIGN KEY (message_id, task_id, expected_revision)
    REFERENCES task_revision_intents(message_id, task_id, expected_revision) ON DELETE RESTRICT,
  FOREIGN KEY (message_id) REFERENCES conversation_message_routes(message_id) ON DELETE RESTRICT,
  FOREIGN KEY (task_id, owner_id, conversation_id)
    REFERENCES tasks(id, owner_id, conversation_id) ON DELETE RESTRICT,
  FOREIGN KEY (conversation_id, owner_id) REFERENCES conversations(id, owner_id) ON DELETE RESTRICT,
  FOREIGN KEY (clarification_id, task_id)
    REFERENCES task_clarification_requests(id, task_id) ON DELETE RESTRICT,
  FOREIGN KEY (task_id, clarification_version)
    REFERENCES task_clarification_requests(task_id, version) ON DELETE RESTRICT,
  CHECK (
    (status = 'pending' AND file_delivery_intent IS NULL AND answer = '' AND responded_at IS NULL) OR
    (status = 'answered' AND file_delivery_intent IS NOT NULL AND answer <> '' AND responded_at IS NOT NULL) OR
    (status = 'cancelled' AND file_delivery_intent IS NULL AND answer = '' AND responded_at IS NOT NULL)
  )
);

CREATE INDEX conversation_route_file_delivery_clarifications_task
  ON conversation_route_file_delivery_clarifications(task_id, expected_revision, status);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM conversation_route_file_delivery_clarifications) THEN
    RAISE EXCEPTION 'cannot remove route file delivery clarifications after they were persisted';
  END IF;
END $$;
-- +goose StatementEnd

DROP INDEX conversation_route_file_delivery_clarifications_task;
DROP TABLE conversation_route_file_delivery_clarifications;
ALTER TABLE task_clarification_requests DROP CONSTRAINT task_clarification_requests_id_task_unique;
ALTER TABLE task_revision_intents DROP CONSTRAINT task_revision_intents_message_revision_unique;
