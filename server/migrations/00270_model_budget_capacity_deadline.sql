-- +goose Up
CREATE TABLE model_budget_capacity_call_deadlines (
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
  project_id                  text NOT NULL DEFAULT '',
  dsh_call_id                 text NOT NULL CHECK (dsh_call_id ~ '^[A-Za-z0-9_-]{8,128}$'),
  ledger_call_id              text NOT NULL CHECK (ledger_call_id ~ '^[A-Za-z0-9_-]{8,128}$'),
  budget_call_id              text NOT NULL CHECK (budget_call_id ~ '^[A-Za-z0-9_-]{8,128}$'),
  request_digest              bytea NOT NULL CHECK (octet_length(request_digest) = 32),
  checkpoint_digest           bytea NOT NULL CHECK (octet_length(checkpoint_digest) = 32),
  first_wait_at               timestamptz NOT NULL,
  deadline_at                 timestamptz NOT NULL,
  PRIMARY KEY (owner_id, budget_call_id),
  UNIQUE (owner_id, ledger_call_id),
  UNIQUE (owner_id, dsh_call_id),
  FOREIGN KEY (task_id, task_owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE,
  CHECK (deadline_at > first_wait_at AND deadline_at <= first_wait_at + interval '5 minutes')
);

CREATE INDEX model_budget_capacity_call_deadlines_task
  ON model_budget_capacity_call_deadlines(task_id, first_wait_at, budget_call_id);

-- Preserve waiters created by 00269 during a rolling deployment. Keep their
-- episode expires_at unchanged; the durable call deadline carries that
-- original deadline across future re-waits without exceeding five minutes.
INSERT INTO model_budget_capacity_call_deadlines
(owner_id,task_id,task_owner_id,task_revision,authorization_version,lease_owner,lease_version,agent_instance_id,agent_generation,
 agent_session_id,agent_root_session_id,engine_version,profile_version,through_sequence,recovery_call_id,recovery_dsh_call_id,
 model_id,project_id,dsh_call_id,ledger_call_id,budget_call_id,request_digest,checkpoint_digest,first_wait_at,deadline_at)
SELECT owner_id,task_id,task_owner_id,task_revision,authorization_version,lease_owner,lease_version,agent_instance_id,agent_generation,
 agent_session_id,agent_root_session_id,engine_version,profile_version,through_sequence,recovery_call_id,recovery_dsh_call_id,
 model_id,COALESCE(project_id,''),dsh_call_id,ledger_call_id,budget_call_id,request_digest,checkpoint_digest,created_at,
 expires_at
FROM model_budget_capacity_waits;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM model_budget_capacity_call_deadlines) THEN
    RAISE EXCEPTION 'cannot roll back while model budget capacity deadlines exist';
  END IF;
END
$$;
-- +goose StatementEnd

DROP INDEX model_budget_capacity_call_deadlines_task;
DROP TABLE model_budget_capacity_call_deadlines;
