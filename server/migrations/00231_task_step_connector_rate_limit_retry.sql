-- +goose Up
ALTER TABLE task_steps DROP CONSTRAINT task_steps_failure_reason_check;
ALTER TABLE task_steps ADD CONSTRAINT task_steps_failure_reason_check CHECK (
  failure_reason IS NULL OR failure_reason IN (
    'not_dispatched', 'tool_failed_after_dispatch', 'side_effect_unknown',
    'authorization_changed', 'resource_changed', 'connector_rate_limited'
  )
);

ALTER TABLE task_steps DROP CONSTRAINT task_steps_retryable_evidence_check;
ALTER TABLE task_steps ADD CONSTRAINT task_steps_retryable_evidence_check CHECK (
  NOT retryable OR
  (failure_reason = 'not_dispatched' AND phase = 'prepare') OR
  (failure_reason = 'connector_rate_limited' AND kind = 'tool' AND status = 'failed' AND
   phase = 'execute' AND tool_name = 'mcp_call')
);

ALTER TABLE task_steps ADD CONSTRAINT task_steps_connector_rate_limit_evidence_check CHECK (
  failure_reason <> 'connector_rate_limited' OR
  (retryable AND kind = 'tool' AND status = 'failed' AND phase = 'execute' AND tool_name = 'mcp_call')
);

CREATE INDEX task_mcp_connector_operations_call_id_lookup_idx
  ON task_mcp_connector_operations (task_id, owner_id, call_id)
  WHERE call_id IS NOT NULL;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM task_steps WHERE failure_reason='connector_rate_limited') THEN
    RAISE EXCEPTION 'cannot remove connector rate-limit retry evidence while such Task steps exist';
  END IF;
END $$;
-- +goose StatementEnd

DROP INDEX task_mcp_connector_operations_call_id_lookup_idx;
ALTER TABLE task_steps DROP CONSTRAINT task_steps_connector_rate_limit_evidence_check;
ALTER TABLE task_steps DROP CONSTRAINT task_steps_retryable_evidence_check;
ALTER TABLE task_steps ADD CONSTRAINT task_steps_retryable_evidence_check CHECK (
  NOT retryable OR failure_reason = 'not_dispatched'
);
ALTER TABLE task_steps DROP CONSTRAINT task_steps_failure_reason_check;
ALTER TABLE task_steps ADD CONSTRAINT task_steps_failure_reason_check CHECK (
  failure_reason IS NULL OR failure_reason IN (
    'not_dispatched', 'tool_failed_after_dispatch', 'side_effect_unknown',
    'authorization_changed', 'resource_changed'
  )
);
