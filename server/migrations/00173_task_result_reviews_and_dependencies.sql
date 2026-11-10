-- +goose Up
ALTER TABLE task_agent_sessions
  ADD COLUMN task_revision bigint NOT NULL DEFAULT 0 CHECK (task_revision >= 0);
UPDATE task_agent_sessions s SET task_revision=t.current_revision
FROM tasks t WHERE t.id=s.task_id AND s.parent_session_id IS NOT NULL;

CREATE TABLE task_dependencies (
  task_id text NOT NULL,
  owner_id text NOT NULL,
  conversation_id text NOT NULL,
  dependency_task_id text NOT NULL,
  required_revision bigint NOT NULL CHECK (required_revision >= 0),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (task_id, dependency_task_id),
  FOREIGN KEY (task_id, owner_id, conversation_id)
    REFERENCES tasks(id, owner_id, conversation_id) ON DELETE CASCADE,
  FOREIGN KEY (dependency_task_id, owner_id, conversation_id)
    REFERENCES tasks(id, owner_id, conversation_id) ON DELETE RESTRICT,
  CHECK (task_id <> dependency_task_id)
);
CREATE INDEX task_dependencies_target ON task_dependencies(dependency_task_id, required_revision);

ALTER TABLE conversation_result_batches
  DROP CONSTRAINT conversation_result_batches_state_check,
  ADD CONSTRAINT conversation_result_batches_state_check
    CHECK (state IN ('pending','processing','published','failed','superseded')),
  ADD COLUMN trigger_message_id text UNIQUE,
  ADD CONSTRAINT conversation_result_trigger_message_fkey
    FOREIGN KEY (trigger_message_id, owner_id, conversation_id, actor_id)
    REFERENCES conversation_messages(id, owner_id, conversation_id, actor_id) ON DELETE RESTRICT;

ALTER TABLE conversation_result_sources
  ADD COLUMN payload_sha256 bytea,
  ADD COLUMN authorization_version bigint;
UPDATE conversation_result_sources s SET payload_sha256=sha256(convert_to(s.content,'UTF8')),
  authorization_version=t.auth_version
FROM tasks t WHERE t.id=s.task_id;
ALTER TABLE conversation_result_sources
  ALTER COLUMN payload_sha256 SET NOT NULL,
  ALTER COLUMN authorization_version SET NOT NULL,
  ADD CONSTRAINT conversation_result_payload_sha256 CHECK (octet_length(payload_sha256)=32),
  ADD CONSTRAINT conversation_result_authorization_version CHECK (authorization_version > 0);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM task_dependencies)
     OR EXISTS (SELECT 1 FROM conversation_result_batches WHERE state IN ('processing','failed') OR trigger_message_id IS NOT NULL)
     OR EXISTS (SELECT 1 FROM conversation_result_sources WHERE payload_sha256 IS NOT NULL OR authorization_version IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot discard task dependency or result review evidence';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE conversation_result_sources
  DROP CONSTRAINT conversation_result_authorization_version,
  DROP CONSTRAINT conversation_result_payload_sha256,
  DROP COLUMN authorization_version,
  DROP COLUMN payload_sha256;
ALTER TABLE conversation_result_batches
  DROP CONSTRAINT conversation_result_trigger_message_fkey,
  DROP COLUMN trigger_message_id,
  DROP CONSTRAINT conversation_result_batches_state_check,
  ADD CONSTRAINT conversation_result_batches_state_check CHECK (state IN ('pending','published','superseded'));
DROP INDEX task_dependencies_target;
DROP TABLE task_dependencies;
ALTER TABLE task_agent_sessions DROP COLUMN task_revision;
