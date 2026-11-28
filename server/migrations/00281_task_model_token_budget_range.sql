-- +goose Up
ALTER TABLE tasks DROP CONSTRAINT tasks_budget_model_tokens_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_budget_model_tokens_check
  CHECK (budget_model_tokens BETWEEN 1 AND 2147483647);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM tasks WHERE budget_model_tokens > 50000) THEN
    RAISE EXCEPTION 'cannot restore the 50000 Task token ceiling while larger Task budgets exist';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE tasks DROP CONSTRAINT tasks_budget_model_tokens_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_budget_model_tokens_check
  CHECK (budget_model_tokens BETWEEN 1 AND 50000);
