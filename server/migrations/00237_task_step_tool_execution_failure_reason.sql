-- +goose Up
ALTER TABLE task_steps DROP CONSTRAINT task_steps_failure_reason_check;
ALTER TABLE task_steps ADD CONSTRAINT task_steps_failure_reason_check CHECK (
  failure_reason IS NULL OR failure_reason IN (
    'not_dispatched', 'tool_failed_after_dispatch', 'side_effect_unknown',
    'authorization_changed', 'resource_changed', 'connector_rate_limited',
    'tool_execution_failed'
  )
);

ALTER TABLE task_steps ADD CONSTRAINT task_steps_tool_execution_failed_evidence_check CHECK (
  failure_reason <> 'tool_execution_failed' OR
  (kind = 'tool' AND status = 'failed' AND phase = 'execute' AND NOT retryable)
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM task_steps WHERE failure_reason = 'tool_execution_failed') THEN
    RAISE EXCEPTION 'cannot remove tool execution failure evidence while such Task steps exist';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE task_steps DROP CONSTRAINT task_steps_tool_execution_failed_evidence_check;
ALTER TABLE task_steps DROP CONSTRAINT task_steps_failure_reason_check;
ALTER TABLE task_steps ADD CONSTRAINT task_steps_failure_reason_check CHECK (
  failure_reason IS NULL OR failure_reason IN (
    'not_dispatched', 'tool_failed_after_dispatch', 'side_effect_unknown',
    'authorization_changed', 'resource_changed', 'connector_rate_limited'
  )
);
