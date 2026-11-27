-- +goose Up
CREATE TABLE task_model_recovery_schedules (
  task_id                       text NOT NULL,
  owner_id                      text NOT NULL,
  initiator_id                  text NOT NULL,
  task_revision                 bigint NOT NULL CHECK (task_revision >= 0),
  authorization_version         bigint NOT NULL CHECK (authorization_version > 0),
  agent_instance_id             text NOT NULL CHECK (agent_instance_id <> ''),
  agent_generation              bigint NOT NULL CHECK (agent_generation > 0),
  agent_session_id              text NOT NULL CHECK (agent_session_id <> ''),
  failed_lease_owner             text NOT NULL CHECK (failed_lease_owner <> ''),
  failed_lease_version           bigint NOT NULL CHECK (failed_lease_version > 0),
  engine_version                text NOT NULL CHECK (engine_version <> ''),
  profile_version               text NOT NULL CHECK (profile_version <> ''),
  failed_call_id                text NOT NULL CHECK (char_length(failed_call_id) BETWEEN 8 AND 128),
  failed_model_id               text NOT NULL CHECK (char_length(failed_model_id) BETWEEN 8 AND 128),
  failed_request_digest         bytea NOT NULL CHECK (octet_length(failed_request_digest) = 32),
  failed_through_seq            bigint NOT NULL CHECK (failed_through_seq >= 0),
  checkpoint_through_seq        bigint NOT NULL CHECK (checkpoint_through_seq >= 0),
  checkpoint_digest             bytea NOT NULL CHECK (octet_length(checkpoint_digest) = 32),
  toolset_digest                bytea NOT NULL CHECK (octet_length(toolset_digest) = 32),
  recovery_call_id              text NOT NULL CHECK (char_length(recovery_call_id) BETWEEN 8 AND 128),
  state                         text NOT NULL CHECK (state IN ('retry_available', 'scheduled', 'attempting', 'spent', 'completed', 'superseded')),
  automatic                     boolean NOT NULL,
  event_sequence                bigint NOT NULL CHECK (event_sequence > 0),
  attempt_started_at            timestamptz,
  created_at                    timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at                    timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (task_id, task_revision),
  UNIQUE (owner_id, recovery_call_id),
  FOREIGN KEY (task_id, owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE,
  CHECK (checkpoint_through_seq = failed_through_seq),
  CHECK (recovery_call_id <> failed_call_id),
  CHECK (
    (state = 'retry_available' AND automatic = false AND attempt_started_at IS NULL) OR
    (state = 'scheduled' AND attempt_started_at IS NULL) OR
    (state IN ('attempting', 'spent', 'completed') AND attempt_started_at IS NOT NULL) OR
    state = 'superseded'
  )
);

CREATE INDEX task_model_recovery_schedules_active
  ON task_model_recovery_schedules(task_id, task_revision, state)
  WHERE state IN ('retry_available', 'scheduled', 'attempting');

-- +goose Down
DROP INDEX task_model_recovery_schedules_active;
DROP TABLE task_model_recovery_schedules;
