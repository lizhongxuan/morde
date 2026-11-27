-- +goose Up
ALTER TABLE model_budget_preparations
  ADD CONSTRAINT model_budget_preparations_call_plan_unique
    UNIQUE (owner_id, source, call_id, plan_id);

CREATE TABLE model_budget_overage_reconciliations (
  owner_id                text NOT NULL,
  source                  text NOT NULL DEFAULT 'task' CHECK (source = 'task'),
  call_id                 text NOT NULL,
  plan_id                 text NOT NULL,
  budget_call_id          text NOT NULL CHECK (budget_call_id ~ '^[A-Za-z0-9_-]{8,128}$'),
  task_id                 text NOT NULL CHECK (task_id ~ '^q[a-z0-9]{7}$'),
  reason_code             text NOT NULL CHECK (reason_code IN ('input_bound_exceeded', 'output_cap_exceeded', 'input_and_output_exceeded')),
  reserved_input_tokens   bigint NOT NULL CHECK (reserved_input_tokens >= 0),
  reserved_output_tokens  bigint NOT NULL CHECK (reserved_output_tokens > 0),
  actual_input_tokens     bigint NOT NULL CHECK (actual_input_tokens >= 0),
  actual_output_tokens    bigint NOT NULL CHECK (actual_output_tokens >= 0),
  input_overage_tokens    bigint GENERATED ALWAYS AS (GREATEST(actual_input_tokens - reserved_input_tokens, 0)) STORED,
  output_overage_tokens   bigint GENERATED ALWAYS AS (GREATEST(actual_output_tokens - reserved_output_tokens, 0)) STORED,
  project_id              text REFERENCES projects(id) ON DELETE RESTRICT,
  currency                text CHECK (currency IS NULL OR currency ~ '^[A-Z]{3}$'),
  reserved_micros         bigint CHECK (reserved_micros IS NULL OR reserved_micros > 0),
  actual_micros           bigint CHECK (actual_micros IS NULL OR actual_micros >= 0),
  money_overage_micros    bigint GENERATED ALWAYS AS (
    CASE WHEN actual_micros IS NULL OR reserved_micros IS NULL THEN NULL
         ELSE GREATEST(actual_micros - reserved_micros, 0) END
  ) STORED,
  reconciliation_status  text NOT NULL DEFAULT 'open' CHECK (reconciliation_status IN ('open', 'resolved')),
  recorded_at             timestamptz NOT NULL DEFAULT clock_timestamp(),
  resolved_at             timestamptz,
  PRIMARY KEY (owner_id, source, call_id),
  FOREIGN KEY (owner_id, source, call_id, plan_id)
    REFERENCES model_budget_preparations(owner_id, source, call_id, plan_id) ON DELETE RESTRICT,
  CHECK (actual_input_tokens > reserved_input_tokens OR actual_output_tokens > reserved_output_tokens),
  CHECK (
    (project_id IS NULL AND currency IS NULL AND reserved_micros IS NULL AND actual_micros IS NULL) OR
    (project_id IS NOT NULL AND currency IS NOT NULL AND reserved_micros IS NOT NULL AND actual_micros IS NOT NULL)
  ),
  CHECK ((reconciliation_status = 'resolved') = (resolved_at IS NOT NULL))
);

CREATE INDEX model_budget_overage_reconciliations_open_idx
  ON model_budget_overage_reconciliations(recorded_at, owner_id, call_id)
  WHERE reconciliation_status = 'open';

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM model_budget_overage_reconciliations) THEN
    RAISE EXCEPTION 'cannot roll back model budget overage reconciliation records';
  END IF;
END
$$;
-- +goose StatementEnd

DROP INDEX model_budget_overage_reconciliations_open_idx;
DROP TABLE model_budget_overage_reconciliations;
ALTER TABLE model_budget_preparations DROP CONSTRAINT model_budget_preparations_call_plan_unique;
