-- +goose Up
-- A pending batch owns a stable future message ID and its original deadline.
CREATE TABLE conversation_result_batches (
  message_id text PRIMARY KEY CHECK (message_id ~ '^m[a-z0-9]{7}$'),
  service_instance_id uuid NOT NULL REFERENCES service_identity(instance_id) ON DELETE RESTRICT,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  actor_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  conversation_id text NOT NULL,
  state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'published', 'superseded')),
  published_message_id text,
  created_at timestamptz NOT NULL,
  deadline timestamptz NOT NULL CHECK (deadline >= created_at AND deadline <= created_at + interval '2 seconds'),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  UNIQUE (message_id, owner_id, conversation_id, actor_id),
  FOREIGN KEY (conversation_id, owner_id) REFERENCES conversations(id, owner_id) ON DELETE RESTRICT,
  FOREIGN KEY (published_message_id, owner_id, conversation_id, actor_id)
    REFERENCES conversation_messages(id, owner_id, conversation_id, actor_id) ON DELETE RESTRICT,
  CHECK ((state = 'published') = (published_message_id IS NOT NULL)),
  CHECK (published_message_id IS NULL OR published_message_id = message_id)
);
CREATE UNIQUE INDEX conversation_result_one_pending_batch
  ON conversation_result_batches(service_instance_id, owner_id, conversation_id, actor_id) WHERE state='pending';
CREATE INDEX conversation_result_pending_deadline
  ON conversation_result_batches(deadline, message_id) WHERE state='pending';

-- Preserve exact provenance independently of retention of task event history.
CREATE TABLE conversation_result_sources (
  batch_message_id text NOT NULL,
  owner_id text NOT NULL,
  actor_id text NOT NULL,
  conversation_id text NOT NULL,
  user_message_id text NOT NULL,
  task_id text NOT NULL,
  task_revision bigint NOT NULL CHECK (task_revision >= 0),
  event_sequence bigint NOT NULL CHECK (event_sequence > 0),
  turn_id text NOT NULL CHECK (turn_id ~ '^s[a-z0-9]{7}$'),
  content text NOT NULL CHECK (octet_length(content) BETWEEN 1 AND 32768),
  source_order integer NOT NULL CHECK (source_order > 0),
  state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'published', 'superseded')),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (task_id, task_revision),
  UNIQUE (batch_message_id, source_order),
  FOREIGN KEY (batch_message_id, owner_id, conversation_id, actor_id)
    REFERENCES conversation_result_batches(message_id, owner_id, conversation_id, actor_id) ON DELETE RESTRICT,
  FOREIGN KEY (user_message_id, owner_id, conversation_id, actor_id)
    REFERENCES conversation_messages(id, owner_id, conversation_id, actor_id) ON DELETE RESTRICT,
  FOREIGN KEY (task_id, owner_id, conversation_id) REFERENCES tasks(id, owner_id, conversation_id) ON DELETE RESTRICT,
  FOREIGN KEY (task_id, owner_id, actor_id) REFERENCES tasks(id, owner_id, initiator_id) ON DELETE RESTRICT
);

-- A snapshot may participate in a grouped result only with exact evidence.
ALTER TABLE task_assistant_message_snapshots
  ADD COLUMN task_revision bigint CHECK (task_revision >= 0),
  ADD COLUMN result_event_sequence bigint CHECK (result_event_sequence > 0),
  ADD CONSTRAINT task_assistant_snapshot_result_evidence_check
    CHECK (result_event_sequence IS NULL OR task_revision IS NOT NULL);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM conversation_result_batches) OR EXISTS (SELECT 1 FROM conversation_result_sources)
     OR EXISTS (SELECT 1 FROM task_assistant_message_snapshots WHERE task_revision IS NOT NULL OR result_event_sequence IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot discard conversation result aggregation evidence';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE task_assistant_message_snapshots DROP CONSTRAINT task_assistant_snapshot_result_evidence_check;
ALTER TABLE task_assistant_message_snapshots DROP COLUMN result_event_sequence, DROP COLUMN task_revision;
DROP TABLE conversation_result_sources;
DROP INDEX conversation_result_pending_deadline;
DROP INDEX conversation_result_one_pending_batch;
DROP TABLE conversation_result_batches;
