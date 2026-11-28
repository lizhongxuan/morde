-- +goose Up
ALTER TABLE task_model_recovery_schedules
  ADD COLUMN recovery_dsh_call_id text;

UPDATE task_model_recovery_schedules
SET recovery_dsh_call_id = 'mdr_' || encode(sha256(convert_to(
  'morde-task-model-recovery-dsh-v1' || E'\n' || task_id || E'\n' || task_revision::text || E'\n' ||
  failed_call_id || E'\n' || failed_through_seq::text, 'UTF8')), 'hex');

ALTER TABLE task_model_recovery_schedules
  ALTER COLUMN recovery_dsh_call_id SET NOT NULL,
  ADD CONSTRAINT task_model_recovery_schedules_recovery_dsh_call_id_check
    CHECK (recovery_dsh_call_id ~ '^mdr_[a-f0-9]{64}$'),
  ADD CONSTRAINT task_model_recovery_schedules_recovery_dsh_call_id_unique
    UNIQUE (owner_id, recovery_dsh_call_id),
  ADD CONSTRAINT task_model_recovery_schedules_recovery_dsh_call_distinct_check
    CHECK (recovery_dsh_call_id <> failed_call_id AND recovery_dsh_call_id <> recovery_call_id);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM task_model_recovery_schedules) THEN
    RAISE EXCEPTION 'cannot remove durable DSH recovery call identities while recovery records exist';
  END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE task_model_recovery_schedules
  DROP CONSTRAINT task_model_recovery_schedules_recovery_dsh_call_distinct_check,
  DROP CONSTRAINT task_model_recovery_schedules_recovery_dsh_call_id_unique,
  DROP CONSTRAINT task_model_recovery_schedules_recovery_dsh_call_id_check,
  DROP COLUMN recovery_dsh_call_id;
