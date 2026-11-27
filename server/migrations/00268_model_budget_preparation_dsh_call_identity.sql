-- +goose Up
ALTER TABLE model_budget_preparations
  ADD COLUMN dsh_call_id text;

UPDATE model_budget_preparations SET dsh_call_id = call_id;

ALTER TABLE model_budget_preparations
  ALTER COLUMN dsh_call_id SET NOT NULL,
  ADD CONSTRAINT model_budget_preparations_dsh_call_id_check
    CHECK (dsh_call_id ~ '^[A-Za-z0-9_-]{8,128}$');

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM model_budget_preparations WHERE dsh_call_id <> call_id) THEN
    RAISE EXCEPTION 'cannot remove distinct DSH call identity while adaptive recovery plans exist';
  END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE model_budget_preparations
  DROP CONSTRAINT model_budget_preparations_dsh_call_id_check,
  DROP COLUMN dsh_call_id;
