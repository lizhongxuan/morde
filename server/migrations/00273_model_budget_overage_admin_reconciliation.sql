-- +goose Up
ALTER TABLE model_budget_overage_reconciliations
  ADD COLUMN resolved_by text REFERENCES users(id) ON DELETE RESTRICT,
  ADD COLUMN resolution_evidence text,
  ADD COLUMN verified_input_tokens bigint,
  ADD COLUMN verified_output_tokens bigint,
  ADD COLUMN verified_micros bigint;

ALTER TABLE model_budget_overage_reconciliations
  ADD CONSTRAINT model_budget_overage_resolution_check CHECK (
    (reconciliation_status='open' AND resolved_at IS NULL AND resolved_by IS NULL AND resolution_evidence IS NULL
      AND verified_input_tokens IS NULL AND verified_output_tokens IS NULL AND verified_micros IS NULL)
    OR
    (reconciliation_status='resolved' AND resolved_at IS NOT NULL AND resolved_by IS NOT NULL
      AND resolution_evidence IS NOT NULL AND octet_length(resolution_evidence) BETWEEN 8 AND 512
      AND btrim(resolution_evidence) <> '' AND verified_input_tokens IS NOT NULL AND verified_input_tokens >= 0
      AND verified_output_tokens IS NOT NULL AND verified_output_tokens >= 0
      AND ((project_id IS NULL AND verified_micros IS NULL) OR
        (project_id IS NOT NULL AND verified_micros IS NOT NULL AND verified_micros >= 0)))
  );

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM model_budget_overage_reconciliations WHERE reconciliation_status='resolved') THEN
    RAISE EXCEPTION 'cannot remove resolved model budget overage audit details';
  END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE model_budget_overage_reconciliations
  DROP CONSTRAINT model_budget_overage_resolution_check,
  DROP COLUMN verified_micros,
  DROP COLUMN verified_output_tokens,
  DROP COLUMN verified_input_tokens,
  DROP COLUMN resolution_evidence,
  DROP COLUMN resolved_by;
