-- +goose Up
CREATE INDEX task_pending_requests_due
  ON task_pending_requests(expires_at, task_id, id)
  WHERE status='pending' AND expires_at IS NOT NULL;

-- +goose Down
DROP INDEX task_pending_requests_due;
