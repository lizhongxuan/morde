-- +goose Up
ALTER TABLE record_capture_queue
  ADD COLUMN record_id text;

ALTER TABLE record_capture_queue
  ADD CONSTRAINT record_capture_queue_record_fk
    FOREIGN KEY (record_id) REFERENCES record_entries(id);

-- +goose Down
ALTER TABLE record_capture_queue
  DROP CONSTRAINT record_capture_queue_record_fk,
  DROP COLUMN record_id;
