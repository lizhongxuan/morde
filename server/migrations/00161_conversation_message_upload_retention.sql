-- +goose Up
ALTER TABLE conversation_message_uploads
  ADD COLUMN expires_at timestamptz;

UPDATE conversation_message_uploads
SET expires_at = updated_at + interval '24 hours'
WHERE state = 'uploading';

ALTER TABLE conversation_message_uploads
  ADD CONSTRAINT conversation_message_uploads_expiry_state_check
  CHECK ((state = 'uploading' AND expires_at IS NOT NULL) OR (state = 'committed' AND expires_at IS NULL));

CREATE INDEX conversation_message_uploads_expiry_idx
  ON conversation_message_uploads (expires_at)
  WHERE state = 'uploading';

ALTER TABLE conversation_message_upload_chunks
  ALTER COLUMN content DROP NOT NULL;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM conversation_message_upload_chunks WHERE content IS NULL) THEN
    RAISE EXCEPTION 'cannot restore discarded conversation message upload chunk content';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE conversation_message_upload_chunks
  ALTER COLUMN content SET NOT NULL;
DROP INDEX conversation_message_uploads_expiry_idx;
ALTER TABLE conversation_message_uploads
  DROP CONSTRAINT conversation_message_uploads_expiry_state_check,
  DROP COLUMN expires_at;
