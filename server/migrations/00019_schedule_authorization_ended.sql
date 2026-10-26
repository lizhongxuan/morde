-- +goose Up
ALTER TABLE schedule_runs DROP CONSTRAINT schedule_runs_skip_reason_check;
ALTER TABLE schedule_runs ADD CONSTRAINT schedule_runs_skip_reason_check
  CHECK (skip_reason IS NULL OR skip_reason IN ('dst_gap', 'overlap', 'authorization_ended'));

-- +goose Down
ALTER TABLE schedule_runs DROP CONSTRAINT schedule_runs_skip_reason_check;
ALTER TABLE schedule_runs ADD CONSTRAINT schedule_runs_skip_reason_check
  CHECK (skip_reason IS NULL OR skip_reason IN ('dst_gap', 'overlap'));
