-- +goose Up
ALTER TABLE schedules
  ADD COLUMN execution_policy text NOT NULL DEFAULT 'standard'
    CHECK (execution_policy IN ('standard', 'goal_followup_read_only'));

ALTER TABLE schedules
  ADD CONSTRAINT schedules_read_only_goal_followup_check
    CHECK (execution_policy <> 'goal_followup_read_only' OR (host_id IS NULL AND observation_spec IS NULL));

-- +goose Down
ALTER TABLE schedules DROP CONSTRAINT schedules_read_only_goal_followup_check;
ALTER TABLE schedules DROP COLUMN execution_policy;
