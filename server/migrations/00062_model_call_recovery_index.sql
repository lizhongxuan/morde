-- +goose Up
CREATE INDEX model_call_ledger_dispatched_age
  ON model_call_ledger(created_at, owner_id, source, call_id) INCLUDE (task_id)
  WHERE state='dispatched';

-- +goose Down
DROP INDEX model_call_ledger_dispatched_age;
