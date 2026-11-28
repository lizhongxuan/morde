-- +goose Up
-- Long assistant replies are stored in the existing conversation and task
-- snapshots. Keep user messages and grouped task-result source fragments at
-- their existing limits; only assistant reply snapshots/messages grow.
ALTER TABLE task_assistant_message_snapshots
  DROP CONSTRAINT task_assistant_message_snapshots_content_check,
  ADD CONSTRAINT task_assistant_message_snapshots_content_check
    CHECK (octet_length(content) BETWEEN 1 AND 2097152);

ALTER TABLE conversation_messages
  ADD CONSTRAINT conversation_messages_assistant_content_size_check
    CHECK (role <> 'assistant' OR octet_length(content) <= 2097152);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM task_assistant_message_snapshots WHERE octet_length(content) > 32768)
     OR EXISTS (SELECT 1 FROM conversation_messages WHERE role='assistant' AND octet_length(content) > 32768) THEN
    RAISE EXCEPTION 'cannot restore the 32KiB assistant reply limit while larger replies are retained';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE conversation_messages
  DROP CONSTRAINT conversation_messages_assistant_content_size_check;

ALTER TABLE task_assistant_message_snapshots
  DROP CONSTRAINT task_assistant_message_snapshots_content_check,
  ADD CONSTRAINT task_assistant_message_snapshots_content_check
    CHECK (octet_length(content) BETWEEN 1 AND 32768);
