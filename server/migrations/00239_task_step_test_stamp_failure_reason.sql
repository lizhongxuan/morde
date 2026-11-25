-- +goose Up
-- E09: the E03/E07 test-run stamps (test_failed, test_passed,
-- test_suite_passed) are written to task_steps.failure_reason by the app
-- (see taskToolTestFailureReason in internal/app/task_step_detail.go and
-- validTaskToolCallFailureEvidence, which accepts them as legitimate step
-- evidence), but migration 00237's check predates the stamps and forbids
-- them, so persisting a test-stamped step violates the check. The
-- independent verifier (E09) re-derives the E07 gate from these stamps,
-- so the check must admit them. Widening only; no data changes.
ALTER TABLE task_steps DROP CONSTRAINT task_steps_failure_reason_check;
ALTER TABLE task_steps ADD CONSTRAINT task_steps_failure_reason_check CHECK (
  failure_reason IS NULL OR failure_reason IN (
    'not_dispatched', 'tool_failed_after_dispatch', 'side_effect_unknown',
    'authorization_changed', 'resource_changed', 'connector_rate_limited',
    'tool_execution_failed', 'test_failed', 'test_passed', 'test_suite_passed'
  )
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM task_steps WHERE failure_reason IN ('test_failed', 'test_passed', 'test_suite_passed')) THEN
    RAISE EXCEPTION 'cannot remove recorded test-run Task steps';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE task_steps DROP CONSTRAINT task_steps_failure_reason_check;
ALTER TABLE task_steps ADD CONSTRAINT task_steps_failure_reason_check CHECK (
  failure_reason IS NULL OR failure_reason IN (
    'not_dispatched', 'tool_failed_after_dispatch', 'side_effect_unknown',
    'authorization_changed', 'resource_changed', 'connector_rate_limited',
    'tool_execution_failed'
  )
);
