-- +goose Up
ALTER TABLE task_steps DROP CONSTRAINT task_steps_status_check;
ALTER TABLE task_steps ADD CONSTRAINT task_steps_status_check CHECK (
  status IN ('queued', 'running', 'waiting_user', 'succeeded', 'failed', 'cancelled', 'unknown')
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM task_steps WHERE status='cancelled') THEN
    RAISE EXCEPTION 'cannot remove recorded cancelled Task steps';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE task_steps DROP CONSTRAINT task_steps_status_check;
ALTER TABLE task_steps ADD CONSTRAINT task_steps_status_check CHECK (
  status IN ('queued', 'running', 'waiting_user', 'succeeded', 'failed', 'unknown')
);
