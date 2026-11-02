-- +goose Up
ALTER TABLE record_capture_queue
  DROP CONSTRAINT record_capture_queue_payload_check,
  ADD CONSTRAINT record_capture_queue_payload_check
    CHECK (octet_length(payload) BETWEEN 0 AND 33554432);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM record_capture_queue WHERE octet_length(payload) = 0) THEN
    RAISE EXCEPTION 'cannot restore non-empty queued payload constraint while cleared queue items exist';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE record_capture_queue
  DROP CONSTRAINT record_capture_queue_payload_check,
  ADD CONSTRAINT record_capture_queue_payload_check
    CHECK (octet_length(payload) BETWEEN 1 AND 33554432);
