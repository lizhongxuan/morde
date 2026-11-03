-- +goose Up
CREATE TABLE admin_model_budget_defaults (
  id integer PRIMARY KEY CHECK (id = 1),
  token_limit bigint NOT NULL CHECK (token_limit > 0),
  max_concurrent integer NOT NULL CHECK (max_concurrent BETWEEN 1 AND 100),
  version bigint NOT NULL CHECK (version > 0),
  updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM admin_model_budget_defaults) THEN
    RAISE EXCEPTION 'cannot roll back a configured default model budget';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE admin_model_budget_defaults;
