-- +goose Up
-- 00269 was deployed before its source migration was extended with these
-- fields. Normalize that already-applied schema forward without changing the
-- published 00269 migration. A legacy waiter can only be backfilled from the
-- exact checkpoint identity that admitted it; never invent a digest.
ALTER TABLE model_budget_capacity_waits
  ADD COLUMN IF NOT EXISTS checkpoint_digest bytea;
ALTER TABLE model_budget_capacity_waits
  ADD COLUMN IF NOT EXISTS last_reconciled_at timestamptz;

UPDATE model_budget_capacity_waits AS wait
SET checkpoint_digest = session.checkpoint_digest
FROM task_agent_sessions AS session
WHERE wait.checkpoint_digest IS NULL
  AND session.task_id = wait.task_id
  AND session.owner_id = wait.task_owner_id
  AND session.session_id = wait.agent_session_id
  AND session.checkpoint_through_seq = wait.through_sequence
  AND octet_length(session.checkpoint_digest) = 32
  AND EXISTS (
    SELECT 1 FROM task_agent_checkpoints AS checkpoint
    JOIN task_agent_instances AS instance
      ON instance.task_id = checkpoint.task_id
     AND instance.owner_id = checkpoint.owner_id
     AND instance.agent_generation = checkpoint.agent_generation
     AND instance.agent_instance_id = checkpoint.agent_instance_id
     AND instance.agent_session_id = checkpoint.agent_root_session_id
    WHERE checkpoint.task_id = wait.task_id
      AND checkpoint.owner_id = wait.task_owner_id
      AND checkpoint.agent_instance_id = wait.agent_instance_id
      AND checkpoint.agent_generation = wait.agent_generation
      AND checkpoint.agent_session_id = wait.agent_session_id
      AND checkpoint.agent_root_session_id = wait.agent_root_session_id
      AND checkpoint.through_seq = wait.through_sequence
      AND checkpoint.digest = session.checkpoint_digest
      AND checkpoint.auth_version = wait.authorization_version
      AND checkpoint.engine_version = wait.engine_version
      AND checkpoint.profile_version = wait.profile_version
      AND checkpoint.lease_version = wait.lease_version
      AND instance.state = 'stopped'
      AND instance.stop_receipt IS NOT NULL
      AND instance.stop_receipt_digest = sha256(instance.stop_receipt)
  );

-- A row without an exact durable checkpoint cannot be safely resumed or
-- converted into a durable deadline. Abort the migration and preserve state.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM model_budget_capacity_waits
    WHERE checkpoint_digest IS NULL OR octet_length(checkpoint_digest) <> 32
  ) THEN
    RAISE EXCEPTION 'cannot migrate capacity waiters without an exact 32-byte Task Agent checkpoint';
  END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE model_budget_capacity_waits
  ALTER COLUMN checkpoint_digest SET NOT NULL;

-- +goose StatementBegin
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conrelid = 'model_budget_capacity_waits'::regclass
      AND conname = 'model_budget_capacity_waits_checkpoint_digest_check'
  ) THEN
    ALTER TABLE model_budget_capacity_waits
      ADD CONSTRAINT model_budget_capacity_waits_checkpoint_digest_check
      CHECK (octet_length(checkpoint_digest) = 32);
  END IF;
END
$$;
-- +goose StatementEnd

CREATE INDEX IF NOT EXISTS model_budget_capacity_waits_owner_fair
  ON model_budget_capacity_waits(owner_id, (COALESCE(last_reconciled_at, created_at)), created_at, task_id);

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

-- Keep the 00269 compatibility columns and owner-fair index. Some databases
-- had already applied the published 00269 before those fields were added to
-- its source file; leaving the additive repair in place is safe when rolling
-- back to version 269 and avoids a schema downgrade that the runtime cannot
-- distinguish.
