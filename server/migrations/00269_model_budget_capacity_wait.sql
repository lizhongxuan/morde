-- +goose Up
CREATE TABLE model_budget_capacity_waits (
  owner_id                    text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  task_id                     text NOT NULL,
  task_owner_id               text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  task_revision               bigint NOT NULL CHECK (task_revision >= 0),
  authorization_version       bigint NOT NULL CHECK (authorization_version > 0),
  lease_owner                 text NOT NULL CHECK (length(lease_owner) BETWEEN 1 AND 128),
  lease_version               bigint NOT NULL CHECK (lease_version > 0),
  agent_instance_id           text NOT NULL CHECK (agent_instance_id ~ '^[A-Za-z0-9_-]{1,128}$'),
  agent_generation            bigint NOT NULL CHECK (agent_generation > 0),
  agent_session_id            text NOT NULL CHECK (agent_session_id ~ '^[A-Za-z0-9_-]{1,128}$'),
  agent_root_session_id       text NOT NULL CHECK (agent_root_session_id ~ '^[A-Za-z0-9_-]{1,128}$'),
  engine_version              text NOT NULL CHECK (length(engine_version) BETWEEN 1 AND 128),
  profile_version             text NOT NULL CHECK (length(profile_version) BETWEEN 1 AND 128),
  through_sequence            bigint NOT NULL CHECK (through_sequence >= 0),
  recovery_call_id            text NOT NULL DEFAULT '' CHECK (recovery_call_id = '' OR recovery_call_id ~ '^[A-Za-z0-9_-]{8,128}$'),
  recovery_dsh_call_id        text NOT NULL DEFAULT '' CHECK (recovery_dsh_call_id = '' OR recovery_dsh_call_id ~ '^[A-Za-z0-9_-]{8,128}$'),
  model_id                    text NOT NULL CHECK (model_id ~ '^p[a-z0-9]{7}$'),
  project_id                  text REFERENCES projects(id) ON DELETE RESTRICT,
  dsh_call_id                 text NOT NULL CHECK (dsh_call_id ~ '^[A-Za-z0-9_-]{8,128}$'),
  ledger_call_id              text NOT NULL CHECK (ledger_call_id ~ '^[A-Za-z0-9_-]{8,128}$'),
  budget_call_id              text NOT NULL CHECK (budget_call_id ~ '^[A-Za-z0-9_-]{8,128}$'),
  request_digest              bytea NOT NULL CHECK (octet_length(request_digest) = 32),
  input_token_reservation      bigint NOT NULL CHECK (input_token_reservation BETWEEN 0 AND 2000000),
  minimum_output_tokens       bigint NOT NULL CHECK (minimum_output_tokens BETWEEN 1 AND 2000000),
  minimum_project_micros      bigint NOT NULL CHECK (minimum_project_micros >= 0),
  project_budget_currency     text NOT NULL DEFAULT '' CHECK (project_budget_currency = '' OR project_budget_currency ~ '^[A-Z]{3}$'),
  created_at                  timestamptz NOT NULL DEFAULT clock_timestamp(),
  expires_at                  timestamptz NOT NULL,
  PRIMARY KEY (task_id),
  FOREIGN KEY (task_id, task_owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE,
  UNIQUE (owner_id, ledger_call_id),
  UNIQUE (owner_id, dsh_call_id),
  UNIQUE (owner_id, budget_call_id),
  CHECK ((recovery_call_id = '') = (recovery_dsh_call_id = '')),
  CHECK ((minimum_project_micros = 0 AND project_budget_currency = '') OR
         (minimum_project_micros > 0 AND project_budget_currency <> '')),
  CHECK (project_id IS NOT NULL OR (minimum_project_micros = 0 AND project_budget_currency = '')),
  CHECK (expires_at > created_at AND expires_at <= created_at + interval '5 minutes')
);

CREATE INDEX model_budget_capacity_waits_fifo
  ON model_budget_capacity_waits(created_at, owner_id, task_id);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM model_budget_capacity_waits) THEN
    RAISE EXCEPTION 'cannot roll back while model budget capacity waiters exist';
  END IF;
END
$$;
-- +goose StatementEnd

DROP INDEX model_budget_capacity_waits_fifo;
DROP TABLE model_budget_capacity_waits;
