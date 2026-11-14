-- +goose Up
ALTER TABLE task_steps
  ADD COLUMN attempt_no bigint NOT NULL DEFAULT 1 CHECK (attempt_no > 0),
  ADD COLUMN retry_of_step_id text,
  ADD COLUMN failure_reason text,
  ADD COLUMN retryable boolean NOT NULL DEFAULT false,
  ADD CONSTRAINT task_steps_retry_of_fk FOREIGN KEY (task_id, retry_of_step_id)
    REFERENCES task_steps(task_id, id) ON DELETE CASCADE,
  ADD CONSTRAINT task_steps_failure_reason_check CHECK (
    failure_reason IS NULL OR failure_reason IN ('not_dispatched', 'tool_failed_after_dispatch', 'side_effect_unknown', 'authorization_changed', 'resource_changed')
  ),
  ADD CONSTRAINT task_steps_retry_attempt_check CHECK (
    (attempt_no = 1 AND retry_of_step_id IS NULL) OR (attempt_no > 1 AND retry_of_step_id IS NOT NULL)
  ),
  ADD CONSTRAINT task_steps_retryable_evidence_check CHECK (
    NOT retryable OR failure_reason = 'not_dispatched'
  );

CREATE TABLE task_step_retry_intents (
  attempt_step_id                text PRIMARY KEY,
  task_id                        text NOT NULL,
  owner_id                       text NOT NULL,
  source_step_id                 text NOT NULL,
  attempt_no                     bigint NOT NULL CHECK (attempt_no > 1),
  task_revision                  bigint NOT NULL CHECK (task_revision >= 0),
  agent_session_id               text NOT NULL,
  source_call_id                 text NOT NULL,
  retry_call_id                  text NOT NULL,
  tool_name                      text NOT NULL,
  request_digest                 bytea NOT NULL CHECK (octet_length(request_digest) = 32),
  authorization_version          bigint NOT NULL CHECK (authorization_version > 0),
  host_id                        text NOT NULL,
  host_runtime_generation        bigint NOT NULL CHECK (host_runtime_generation > 0),
  workspace_id                   text,
  input_revision                 bigint,
  workspace_checkpoint_revision  bigint,
  workspace_binding_version      bigint NOT NULL DEFAULT 0,
  workspace_revision             bigint,
  checkpoint_through_seq         bigint NOT NULL CHECK (checkpoint_through_seq >= 0),
  checkpoint_digest              bytea NOT NULL CHECK (octet_length(checkpoint_digest) = 32),
  state                          text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'consumed')),
  expires_at                     timestamptz NOT NULL,
  consumed_at                    timestamptz,
  created_at                     timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at                     timestamptz NOT NULL DEFAULT clock_timestamp(),
  FOREIGN KEY (task_id, owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE,
  FOREIGN KEY (task_id, source_step_id) REFERENCES task_steps(task_id, id) ON DELETE CASCADE,
  FOREIGN KEY (task_id, attempt_step_id) REFERENCES task_steps(task_id, id) ON DELETE CASCADE,
  UNIQUE (task_id, retry_call_id),
  CHECK ((state = 'pending' AND consumed_at IS NULL) OR (state = 'consumed' AND consumed_at IS NOT NULL)),
  CHECK ((workspace_id IS NULL AND input_revision IS NULL AND workspace_checkpoint_revision IS NULL AND
          workspace_binding_version = 0 AND workspace_revision IS NULL) OR
         (workspace_id IS NOT NULL AND input_revision IS NOT NULL AND workspace_checkpoint_revision IS NOT NULL AND
          workspace_binding_version > 0 AND workspace_revision IS NOT NULL))
);

CREATE UNIQUE INDEX task_step_retry_intents_one_pending_source
  ON task_step_retry_intents(task_id, source_step_id) WHERE state='pending';
CREATE INDEX task_step_retry_intents_expiry
  ON task_step_retry_intents(expires_at) WHERE state='pending';

-- +goose Down
DROP INDEX task_step_retry_intents_expiry;
DROP INDEX task_step_retry_intents_one_pending_source;
DROP TABLE task_step_retry_intents;
ALTER TABLE task_steps DROP CONSTRAINT task_steps_failure_reason_check,
  DROP CONSTRAINT task_steps_retryable_evidence_check,
  DROP CONSTRAINT task_steps_retry_attempt_check,
  DROP CONSTRAINT task_steps_retry_of_fk,
  DROP COLUMN retryable,
  DROP COLUMN failure_reason,
  DROP COLUMN retry_of_step_id,
  DROP COLUMN attempt_no;
