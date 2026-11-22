-- +goose Up
ALTER TABLE model_call_ledger DROP CONSTRAINT model_call_ledger_state_check;
ALTER TABLE model_call_ledger ADD CONSTRAINT model_call_ledger_state_check
  CHECK (state IN ('dispatched', 'settled', 'unknown', 'rejected'));

ALTER TABLE project_budget_ledger DROP CONSTRAINT project_budget_ledger_status_check;
ALTER TABLE project_budget_ledger ADD CONSTRAINT project_budget_ledger_status_check
  CHECK (status IN ('reserved', 'settled', 'unknown', 'rejected'));
ALTER TABLE project_budget_ledger DROP CONSTRAINT project_budget_ledger_check5;
ALTER TABLE project_budget_ledger ADD CONSTRAINT project_budget_ledger_usage_status_check CHECK (
  (status IN ('reserved', 'unknown', 'rejected') AND actual_input_tokens IS NULL) OR
  (status = 'settled' AND actual_input_tokens IS NOT NULL AND
    (actual_input_tokens > 0 OR actual_output_tokens > 0))
);

ALTER TABLE conversation_route_model_calls DROP CONSTRAINT conversation_route_model_calls_state_check;
ALTER TABLE conversation_route_model_calls ADD CONSTRAINT conversation_route_model_calls_state_check
  CHECK (state IN ('reserved', 'dispatched', 'settled', 'unknown', 'rejected'));

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM model_call_ledger WHERE state='rejected') OR
     EXISTS (SELECT 1 FROM project_budget_ledger WHERE status='rejected') OR
     EXISTS (SELECT 1 FROM conversation_route_model_calls WHERE state='rejected') THEN
    RAISE EXCEPTION 'cannot discard confirmed model rejection evidence';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE conversation_route_model_calls DROP CONSTRAINT conversation_route_model_calls_state_check;
ALTER TABLE conversation_route_model_calls ADD CONSTRAINT conversation_route_model_calls_state_check
  CHECK (state IN ('reserved', 'dispatched', 'settled', 'unknown'));

ALTER TABLE project_budget_ledger DROP CONSTRAINT project_budget_ledger_usage_status_check;
ALTER TABLE project_budget_ledger DROP CONSTRAINT project_budget_ledger_status_check;
ALTER TABLE project_budget_ledger ADD CONSTRAINT project_budget_ledger_status_check
  CHECK (status IN ('reserved', 'settled', 'unknown'));
ALTER TABLE project_budget_ledger ADD CONSTRAINT project_budget_ledger_check5 CHECK (
  (status IN ('reserved', 'unknown') AND actual_input_tokens IS NULL) OR
  (status = 'settled' AND actual_input_tokens IS NOT NULL AND
    (actual_input_tokens > 0 OR actual_output_tokens > 0))
);

ALTER TABLE model_call_ledger DROP CONSTRAINT model_call_ledger_state_check;
ALTER TABLE model_call_ledger ADD CONSTRAINT model_call_ledger_state_check
  CHECK (state IN ('dispatched', 'settled', 'unknown'));
