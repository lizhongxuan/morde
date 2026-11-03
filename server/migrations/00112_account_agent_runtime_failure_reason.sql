-- +goose Up
ALTER TABLE account_agent_runtimes
  ADD COLUMN failure_reason text NOT NULL DEFAULT '';

UPDATE account_agent_runtimes SET failure_reason='runtime_failed' WHERE state='failed';

ALTER TABLE account_agent_runtimes
  ADD CONSTRAINT account_agent_runtime_failure_reason_check CHECK (
    (state='failed' AND failure_reason IN ('startup_failed', 'runtime_failed'))
    OR (state<>'failed' AND failure_reason='')
  );

-- +goose Down
ALTER TABLE account_agent_runtimes
  DROP CONSTRAINT account_agent_runtime_failure_reason_check,
  DROP COLUMN failure_reason;
