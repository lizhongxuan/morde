-- +goose Up
-- A streamed assistant message reserves its real conversation sequence with
-- its first visible delta. The same row is retained across interruption and
-- is completed only with a checkpointed Task result.
ALTER TABLE task_events ADD COLUMN task_revision bigint CHECK (task_revision >= 0);
ALTER TABLE task_events ADD COLUMN result_content_sha256 bytea CHECK (result_content_sha256 IS NULL OR octet_length(result_content_sha256) = 32);
CREATE UNIQUE INDEX task_events_revision_evidence ON task_events(task_id,sequence,task_revision);
ALTER TABLE conversation_messages
  ADD COLUMN output_status text CHECK (output_status IN ('streaming','interrupted','completed')),
  ADD COLUMN output_task_revision bigint CHECK (output_task_revision >= 0),
  ADD COLUMN output_result_event_sequence bigint CHECK (output_result_event_sequence > 0),
  ADD CONSTRAINT conversation_stream_output_check CHECK (
    (output_status IS NULL AND output_task_revision IS NULL AND output_result_event_sequence IS NULL)
    OR (role='assistant' AND task_id IS NOT NULL AND output_status IS NOT NULL AND output_task_revision IS NOT NULL
      AND ((output_status='completed') = (output_result_event_sequence IS NOT NULL)))
  ),
  ADD CONSTRAINT conversation_stream_result_event_fk FOREIGN KEY (task_id,output_result_event_sequence,output_task_revision)
    REFERENCES task_events(task_id,sequence,task_revision) ON DELETE RESTRICT;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM conversation_messages WHERE output_status IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot discard persisted assistant stream evidence';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE conversation_messages DROP CONSTRAINT conversation_stream_result_event_fk;
ALTER TABLE conversation_messages DROP CONSTRAINT conversation_stream_output_check;
ALTER TABLE conversation_messages DROP COLUMN output_result_event_sequence, DROP COLUMN output_task_revision, DROP COLUMN output_status;
DROP INDEX task_events_revision_evidence;
ALTER TABLE task_events DROP COLUMN task_revision;
ALTER TABLE task_events DROP COLUMN result_content_sha256;
