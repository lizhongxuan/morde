-- +goose Up
-- There is intentionally no default or automatic allowance. An operator must
-- configure each account before it may consume a platform model.
CREATE TABLE model_account_budgets (
  owner_id text PRIMARY KEY REFERENCES users(id),
  token_limit bigint NOT NULL CHECK (token_limit > 0),
  max_concurrent integer NOT NULL CHECK (max_concurrent BETWEEN 1 AND 100),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE model_call_ledger (
  owner_id text NOT NULL REFERENCES users(id),
  source text NOT NULL CHECK (source IN ('client', 'chat', 'task', 'probe')),
  call_id text NOT NULL CHECK (length(call_id) BETWEEN 8 AND 128),
  request_digest bytea NOT NULL CHECK (octet_length(request_digest) = 32),
  model_id text NOT NULL,
  provider_model text NOT NULL CHECK (length(provider_model) BETWEEN 1 AND 256),
  task_id text REFERENCES tasks(id),
  reserved_tokens bigint NOT NULL CHECK (reserved_tokens > 0),
  state text NOT NULL CHECK (state IN ('dispatched', 'settled', 'unknown')),
  input_tokens bigint CHECK (input_tokens >= 0),
  output_tokens bigint CHECK (output_tokens >= 0),
  created_at timestamptz NOT NULL DEFAULT now(),
  settled_at timestamptz,
  PRIMARY KEY (owner_id, source, call_id),
  CHECK ((source = 'task') = (task_id IS NOT NULL)),
  CHECK ((state = 'settled') = (input_tokens IS NOT NULL AND output_tokens IS NOT NULL)),
  CHECK ((state = 'dispatched') = (settled_at IS NULL))
);
CREATE INDEX model_call_ledger_task ON model_call_ledger(task_id) WHERE task_id IS NOT NULL;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM model_call_ledger) OR EXISTS (SELECT 1 FROM model_account_budgets) THEN
    RAISE EXCEPTION 'cannot roll back nonempty model call ledger or budgets';
  END IF;
END;
$$;
-- +goose StatementEnd
DROP TABLE model_call_ledger;
DROP TABLE model_account_budgets;
