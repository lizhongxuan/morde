-- +goose Up
-- E05: approval card body fields on task_pending_requests. All columns are
-- nullable: rows created before this migration have no body fields, and a
-- NULL cost means the cost is unknown (never 0). New sensitive-modification
-- approvals are required to carry the three text fields by the app layer
-- (newPendingApproval / newHostlessWorkspaceApproval fail closed); the
-- store only enforces bounds on values that are present.
ALTER TABLE task_pending_requests
  ADD COLUMN evidence_pointer text CHECK (evidence_pointer IS NULL OR char_length(evidence_pointer) BETWEEN 1 AND 512),
  ADD COLUMN current_cost_micros bigint CHECK (current_cost_micros IS NULL OR current_cost_micros >= 0),
  ADD COLUMN current_cost_currency text CHECK (current_cost_currency IS NULL OR current_cost_currency ~ '^[A-Z]{3}$'),
  ADD COLUMN current_cost_basis text CHECK (current_cost_basis IS NULL OR current_cost_basis IN ('estimate', 'settled')),
  ADD COLUMN total_cost_micros bigint CHECK (total_cost_micros IS NULL OR total_cost_micros >= 0),
  ADD COLUMN total_cost_currency text CHECK (total_cost_currency IS NULL OR total_cost_currency ~ '^[A-Z]{3}$'),
  ADD COLUMN total_cost_basis text CHECK (total_cost_basis IS NULL OR total_cost_basis IN ('estimate', 'settled')),
  ADD COLUMN trigger_rule text CHECK (trigger_rule IS NULL OR char_length(trigger_rule) BETWEEN 1 AND 512),
  ADD COLUMN rejection_consequence text CHECK (rejection_consequence IS NULL OR char_length(rejection_consequence) BETWEEN 1 AND 512);

-- +goose Down
ALTER TABLE task_pending_requests
  DROP COLUMN rejection_consequence,
  DROP COLUMN trigger_rule,
  DROP COLUMN total_cost_basis,
  DROP COLUMN total_cost_currency,
  DROP COLUMN total_cost_micros,
  DROP COLUMN current_cost_basis,
  DROP COLUMN current_cost_currency,
  DROP COLUMN current_cost_micros,
  DROP COLUMN evidence_pointer;
