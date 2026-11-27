-- +goose Up
ALTER TABLE model_call_ledger
  DROP CONSTRAINT model_call_ledger_state_check,
  ADD CONSTRAINT model_call_ledger_state_check
    CHECK (state IN ('prepared', 'dispatched', 'settled', 'unknown', 'rejected'));

-- A prepared budget hold has not reached the provider, just like a dispatched
-- hold it has no settlement timestamp. The meaning of every old state stays
-- unchanged: all terminal/unknown states must carry settled_at.
ALTER TABLE model_call_ledger
  DROP CONSTRAINT model_call_ledger_check2,
  ADD CONSTRAINT model_call_ledger_settled_at_state_check CHECK (
    (state IN ('prepared', 'dispatched') AND settled_at IS NULL) OR
    (state IN ('settled', 'unknown', 'rejected') AND settled_at IS NOT NULL)
  );

ALTER TABLE model_call_ledger
  ADD CONSTRAINT model_call_ledger_budget_identity_unique
    UNIQUE (owner_id, source, call_id, task_id, model_id, provider_model);

CREATE TABLE model_budget_preparations (
  owner_id                         text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  source                           text NOT NULL DEFAULT 'task' CHECK (source = 'task'),
  call_id                          text NOT NULL CHECK (call_id ~ '^[A-Za-z0-9._~-]{8,128}$'),
  task_id                          text NOT NULL CHECK (task_id ~ '^q[a-z0-9]{7}$'),
  task_revision                    bigint NOT NULL CHECK (task_revision >= 0),
  lease_owner                      text NOT NULL CHECK (length(lease_owner) BETWEEN 1 AND 128),
  lease_version                    bigint NOT NULL CHECK (lease_version > 0),
  agent_instance_id                text NOT NULL CHECK (agent_instance_id ~ '^[A-Za-z0-9_-]{1,128}$'),
  agent_generation                 bigint NOT NULL CHECK (agent_generation > 0),
  agent_session_id                 text NOT NULL CHECK (agent_session_id ~ '^[A-Za-z0-9_-]{1,128}$'),
  agent_root_session_id            text NOT NULL CHECK (agent_root_session_id ~ '^[A-Za-z0-9_-]{1,128}$'),
  authorization_version            bigint NOT NULL CHECK (authorization_version > 0),
  engine_version                   text NOT NULL CHECK (length(engine_version) BETWEEN 1 AND 128),
  profile_version                  text NOT NULL CHECK (length(profile_version) BETWEEN 1 AND 128),
  through_sequence                 bigint NOT NULL CHECK (through_sequence >= 0),
  turn                             bigint NOT NULL CHECK (turn >= 0),
  step                             bigint NOT NULL CHECK (step >= 0),
  plan_id                          text NOT NULL UNIQUE CHECK (plan_id ~ '^[A-Za-z0-9_-]{8,128}$'),
  budget_call_id                   text NOT NULL UNIQUE CHECK (budget_call_id ~ '^[A-Za-z0-9_-]{8,128}$'),
  request_digest                   bytea NOT NULL CHECK (octet_length(request_digest) = 32),
  model_id                         text NOT NULL CHECK (model_id ~ '^p[a-z0-9]{7}$'),
  provider                         text NOT NULL CHECK (length(provider) BETWEEN 1 AND 128),
  provider_model                   text NOT NULL CHECK (length(provider_model) BETWEEN 1 AND 256),
  endpoint                         text NOT NULL CHECK (length(endpoint) BETWEEN 1 AND 2048),
  connection_version               timestamptz NOT NULL,
  capability_version              text NOT NULL CHECK (length(capability_version) BETWEEN 1 AND 128),
  stage                            text NOT NULL CHECK (length(stage) BETWEEN 1 AND 64),
  strategy_version                 text NOT NULL CHECK (length(strategy_version) BETWEEN 1 AND 64),
  model_max_output_tokens          bigint NOT NULL CHECK (model_max_output_tokens BETWEEN 1 AND 2000000),
  context_window_tokens            bigint NOT NULL CHECK (context_window_tokens BETWEEN 4096 AND 2000000),
  context_input_upper_bound_tokens bigint NOT NULL CHECK (context_input_upper_bound_tokens BETWEEN 0 AND 2000000),
  context_safety_margin_tokens     bigint NOT NULL CHECK (context_safety_margin_tokens BETWEEN 0 AND 2000000),
  goal_tokens                      bigint NOT NULL CHECK (goal_tokens BETWEEN 1 AND 2000000),
  minimum_tokens                   bigint NOT NULL CHECK (minimum_tokens BETWEEN 1 AND goal_tokens),
  safe_tokens                      bigint NOT NULL CHECK (safe_tokens BETWEEN 1 AND 2000000),
  send_tokens                      bigint NOT NULL CHECK (send_tokens BETWEEN minimum_tokens AND goal_tokens),
  input_reserved_tokens            bigint NOT NULL CHECK (input_reserved_tokens BETWEEN 0 AND 2000000),
  prepared_at                      timestamptz NOT NULL DEFAULT clock_timestamp(),
  expires_at                       timestamptz NOT NULL,
  consumed_at                      timestamptz,
  PRIMARY KEY (owner_id, source, call_id),
  CONSTRAINT model_budget_preparations_ledger_fk
    FOREIGN KEY (owner_id, source, call_id)
    REFERENCES model_call_ledger(owner_id, source, call_id) ON DELETE RESTRICT,
  CONSTRAINT model_budget_preparations_ledger_identity_fk
    FOREIGN KEY (owner_id, source, call_id, task_id, model_id, provider_model)
    REFERENCES model_call_ledger(owner_id, source, call_id, task_id, model_id, provider_model) ON DELETE RESTRICT,
  CONSTRAINT model_budget_preparations_output_contract_check CHECK (
    safe_tokens <= model_max_output_tokens AND
    safe_tokens <= GREATEST(context_window_tokens - context_input_upper_bound_tokens - context_safety_margin_tokens, 0) AND
    send_tokens = LEAST(goal_tokens, safe_tokens)
  ),
  CONSTRAINT model_budget_preparations_expiry_check CHECK (
    expires_at > prepared_at AND expires_at <= prepared_at + interval '5 minutes' AND
    (consumed_at IS NULL OR (consumed_at >= prepared_at AND consumed_at < expires_at))
  )
);

CREATE INDEX model_budget_preparations_expiry_idx
  ON model_budget_preparations(expires_at, owner_id, call_id)
  WHERE consumed_at IS NULL;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM model_budget_preparations) OR
     EXISTS (SELECT 1 FROM model_call_ledger WHERE state = 'prepared') THEN
    RAISE EXCEPTION 'cannot roll back model budget preparations while plan records or prepared model calls exist';
  END IF;
END
$$;
-- +goose StatementEnd

DROP INDEX model_budget_preparations_expiry_idx;
DROP TABLE model_budget_preparations;
ALTER TABLE model_call_ledger DROP CONSTRAINT model_call_ledger_budget_identity_unique;
ALTER TABLE model_call_ledger
  DROP CONSTRAINT model_call_ledger_settled_at_state_check,
  ADD CONSTRAINT model_call_ledger_check2 CHECK (
    (state = 'dispatched') = (settled_at IS NULL)
  );
ALTER TABLE model_call_ledger
  DROP CONSTRAINT model_call_ledger_state_check,
  ADD CONSTRAINT model_call_ledger_state_check
    CHECK (state IN ('dispatched', 'settled', 'unknown', 'rejected'));
