-- +goose Up
ALTER TABLE tasks ADD COLUMN runtime_lease_safe_after timestamptz;

ALTER TABLE task_operations DROP CONSTRAINT task_operations_status_check;
ALTER TABLE task_operations ADD CONSTRAINT task_operations_status_check
  CHECK (status IN ('prepared', 'dispatched', 'succeeded', 'failed', 'cancelled', 'unknown'));

ALTER TABLE task_pending_requests DROP CONSTRAINT task_pending_requests_status_check;
ALTER TABLE task_pending_requests ADD CONSTRAINT task_pending_requests_status_check
  CHECK (status IN ('pending', 'approved', 'rejected', 'cancelled'));
ALTER TABLE task_pending_requests DROP CONSTRAINT task_pending_requests_check;
ALTER TABLE task_pending_requests ADD CONSTRAINT task_pending_requests_resolution_check
  CHECK ((status = 'pending' AND decision = '' AND responded_at IS NULL) OR
         (status = 'approved' AND decision = 'approve' AND responded_at IS NOT NULL) OR
         (status = 'rejected' AND decision = 'reject' AND responded_at IS NOT NULL) OR
         (status = 'cancelled' AND decision = '' AND responded_at IS NOT NULL));

CREATE TABLE task_stop_requests (
  id               text PRIMARY KEY CHECK (id ~ '^x[a-z0-9]{7}$'),
  task_id          text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  action           text NOT NULL CHECK (action IN ('pause', 'cancel')),
  status           text NOT NULL CHECK (status IN ('pending', 'completed')),
  safe_after       timestamptz NOT NULL,
  lease_owner      text NOT NULL DEFAULT '',
  lease_version    bigint NOT NULL DEFAULT 0 CHECK (lease_version >= 0),
  lease_expires_at timestamptz,
  next_attempt_at  timestamptz NOT NULL DEFAULT now(),
  attempts         integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  completed_at     timestamptz,
  UNIQUE (id, task_id),
  CHECK ((lease_owner = '') = (lease_expires_at IS NULL)),
  CHECK ((status = 'pending' AND completed_at IS NULL) OR
         (status = 'completed' AND completed_at IS NOT NULL AND lease_owner = '' AND lease_expires_at IS NULL))
);
CREATE UNIQUE INDEX task_stop_requests_one_pending ON task_stop_requests(task_id) WHERE status='pending';
CREATE INDEX task_stop_requests_claimable ON task_stop_requests(next_attempt_at, lease_expires_at, created_at, id)
  WHERE status='pending';

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM task_stop_requests) THEN
    RAISE EXCEPTION 'cannot roll back task stop migration after any stop request was persisted';
  END IF;
  IF EXISTS (SELECT 1 FROM task_operations WHERE status='cancelled') THEN
    RAISE EXCEPTION 'cannot roll back task stop migration while cancelled operations exist';
  END IF;
  IF EXISTS (SELECT 1 FROM task_pending_requests WHERE status='cancelled') THEN
    RAISE EXCEPTION 'cannot roll back task stop migration while cancelled pending requests exist';
  END IF;
END $$;
-- +goose StatementEnd

DROP TABLE task_stop_requests;
ALTER TABLE task_pending_requests DROP CONSTRAINT task_pending_requests_resolution_check;
ALTER TABLE task_pending_requests DROP CONSTRAINT task_pending_requests_status_check;
ALTER TABLE task_pending_requests ADD CONSTRAINT task_pending_requests_status_check
  CHECK (status IN ('pending', 'approved', 'rejected'));
ALTER TABLE task_pending_requests ADD CONSTRAINT task_pending_requests_check
  CHECK ((status = 'pending' AND decision = '' AND responded_at IS NULL) OR
         (status = 'approved' AND decision = 'approve' AND responded_at IS NOT NULL) OR
         (status = 'rejected' AND decision = 'reject' AND responded_at IS NOT NULL));
ALTER TABLE task_operations DROP CONSTRAINT task_operations_status_check;
ALTER TABLE task_operations ADD CONSTRAINT task_operations_status_check
  CHECK (status IN ('prepared', 'dispatched', 'succeeded', 'failed', 'unknown'));
ALTER TABLE tasks DROP COLUMN runtime_lease_safe_after;
