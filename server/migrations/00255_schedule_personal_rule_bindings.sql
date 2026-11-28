-- +goose Up
ALTER TABLE schedules
  ADD COLUMN personal_rule_binding jsonb
    CHECK (personal_rule_binding IS NULL OR jsonb_typeof(personal_rule_binding) = 'object');

ALTER TABLE schedule_runs
  ADD COLUMN personal_rule_binding jsonb
    CHECK (personal_rule_binding IS NULL OR jsonb_typeof(personal_rule_binding) = 'object');

ALTER TABLE schedule_runs
  ADD COLUMN personal_rule_validation_status text NOT NULL DEFAULT 'not_bound'
    CHECK (personal_rule_validation_status IN ('not_bound', 'validated', 'revoked')),
  ADD COLUMN personal_rule_validated_at timestamptz,
  ADD COLUMN personal_rule_revoked_at timestamptz,
  ADD CONSTRAINT schedule_runs_personal_rule_validation_check CHECK (
    (personal_rule_binding IS NULL AND personal_rule_validation_status='not_bound'
      AND personal_rule_validated_at IS NULL AND personal_rule_revoked_at IS NULL)
    OR (personal_rule_binding IS NOT NULL AND personal_rule_validation_status='validated'
      AND personal_rule_validated_at IS NOT NULL AND personal_rule_revoked_at IS NULL)
    OR (personal_rule_binding IS NOT NULL AND personal_rule_validation_status='revoked'
      AND personal_rule_revoked_at IS NOT NULL)
  );

ALTER TABLE schedule_runs DROP CONSTRAINT schedule_runs_skip_reason_check;
ALTER TABLE schedule_runs ADD CONSTRAINT schedule_runs_skip_reason_check
  CHECK (skip_reason IS NULL OR skip_reason IN ('dst_gap', 'overlap', 'authorization_ended', 'personal_rule_revoked'));

-- +goose Down
ALTER TABLE schedule_runs DROP CONSTRAINT schedule_runs_skip_reason_check;
ALTER TABLE schedule_runs ADD CONSTRAINT schedule_runs_skip_reason_check
  CHECK (skip_reason IS NULL OR skip_reason IN ('dst_gap', 'overlap', 'authorization_ended'));
ALTER TABLE schedule_runs DROP CONSTRAINT schedule_runs_personal_rule_validation_check;
ALTER TABLE schedule_runs DROP COLUMN personal_rule_revoked_at;
ALTER TABLE schedule_runs DROP COLUMN personal_rule_validated_at;
ALTER TABLE schedule_runs DROP COLUMN personal_rule_validation_status;
ALTER TABLE schedule_runs DROP COLUMN personal_rule_binding;
ALTER TABLE schedules DROP COLUMN personal_rule_binding;
