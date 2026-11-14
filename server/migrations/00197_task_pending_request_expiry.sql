-- +goose Up
ALTER TABLE task_pending_requests
  ADD COLUMN expires_at timestamptz;

ALTER TABLE task_pending_requests
  ADD CONSTRAINT task_pending_requests_expiry_after_created
  CHECK (expires_at IS NULL OR expires_at > created_at);

ALTER TABLE task_pending_requests
  DROP CONSTRAINT task_pending_requests_status_check;

ALTER TABLE task_pending_requests
  ADD CONSTRAINT task_pending_requests_status_check
  CHECK (status IN ('pending', 'approved', 'rejected', 'cancelled', 'expired'));

ALTER TABLE task_pending_requests
  DROP CONSTRAINT task_pending_requests_resolution_check;

ALTER TABLE task_pending_requests
  ADD CONSTRAINT task_pending_requests_resolution_check
  CHECK ((status = 'pending' AND decision = '' AND responded_at IS NULL) OR
         (status = 'approved' AND decision = 'approve' AND responded_at IS NOT NULL) OR
         (status = 'rejected' AND decision = 'reject' AND responded_at IS NOT NULL) OR
         (status = 'cancelled' AND decision = '' AND responded_at IS NOT NULL) OR
         (status = 'expired' AND decision = 'reject' AND responded_at IS NOT NULL));

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM task_pending_requests WHERE status='expired') THEN
    RAISE EXCEPTION 'cannot roll back task pending expiry while expired approvals exist';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE task_pending_requests
  DROP CONSTRAINT task_pending_requests_resolution_check;

ALTER TABLE task_pending_requests
  ADD CONSTRAINT task_pending_requests_resolution_check
  CHECK ((status = 'pending' AND decision = '' AND responded_at IS NULL) OR
         (status = 'approved' AND decision = 'approve' AND responded_at IS NOT NULL) OR
         (status = 'rejected' AND decision = 'reject' AND responded_at IS NOT NULL) OR
         (status = 'cancelled' AND decision = '' AND responded_at IS NOT NULL));

ALTER TABLE task_pending_requests
  DROP CONSTRAINT task_pending_requests_status_check;

ALTER TABLE task_pending_requests
  ADD CONSTRAINT task_pending_requests_status_check
  CHECK (status IN ('pending', 'approved', 'rejected', 'cancelled'));

ALTER TABLE task_pending_requests
  DROP CONSTRAINT task_pending_requests_expiry_after_created;

ALTER TABLE task_pending_requests
  DROP COLUMN expires_at;
