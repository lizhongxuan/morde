-- +goose Up
ALTER TABLE task_file_fetches
  ADD COLUMN failure_reason text;

UPDATE task_file_fetches
SET failure_reason='transfer_failed'
WHERE status='failed';

ALTER TABLE task_file_fetches
  ADD CONSTRAINT task_file_fetches_failure_reason_check
    CHECK (failure_reason IS NULL OR failure_reason IN (
      'access_denied', 'source_changed', 'file_unavailable', 'limit_exceeded',
      'unsupported', 'source_unavailable', 'transfer_failed'
    )),
  ADD CONSTRAINT task_file_fetches_failure_status_check
    CHECK (status='failed' OR failure_reason IS NULL);

-- +goose Down
ALTER TABLE task_file_fetches
  DROP CONSTRAINT task_file_fetches_failure_status_check,
  DROP CONSTRAINT task_file_fetches_failure_reason_check,
  DROP COLUMN failure_reason;
