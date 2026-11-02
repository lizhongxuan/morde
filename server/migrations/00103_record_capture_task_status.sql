-- +goose Up
ALTER TABLE record_capture_queue
  ADD COLUMN source_task_id text,
  ADD CONSTRAINT record_capture_queue_source_task_id_check
    CHECK (source_task_id IS NULL OR source_task_id ~ '^q[a-z0-9]{7}$');

CREATE UNIQUE INDEX record_capture_queue_task_source_unique
  ON record_capture_queue (owner_id, source_task_id, purpose)
  WHERE source_task_id IS NOT NULL;

ALTER TABLE record_capture_queue
  DROP CONSTRAINT record_capture_queue_reserved_bytes_check,
  ADD CONSTRAINT record_capture_queue_reserved_bytes_check CHECK (
    reserved_bytes >= 0 AND
    (reserved_bytes > 0 OR (state IN ('failed','rejected') AND octet_length(payload) = 0))
  );

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM record_capture_queue WHERE reserved_bytes = 0) THEN
    RAISE EXCEPTION 'cannot restore positive record-capture reservations while bodyless failure statuses exist';
  END IF;
END $$;
-- +goose StatementEnd

DROP INDEX record_capture_queue_task_source_unique;

ALTER TABLE record_capture_queue
  DROP CONSTRAINT record_capture_queue_reserved_bytes_check,
  ADD CONSTRAINT record_capture_queue_reserved_bytes_check CHECK (reserved_bytes > 0),
  DROP CONSTRAINT record_capture_queue_source_task_id_check,
  DROP COLUMN source_task_id;
