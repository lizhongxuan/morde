-- Persist only complete, validated Task model results beside the existing
-- token ledger so they can be replayed without another provider dispatch.
-- +goose Up
CREATE TABLE task_model_call_responses (
  owner_id                text NOT NULL,
  source                  text NOT NULL DEFAULT 'task' CHECK (source = 'task'),
  call_id                 text NOT NULL,
  task_id                 text NOT NULL,
  task_owner_id           text NOT NULL,
  initiator_id            text NOT NULL CHECK (initiator_id = owner_id),
  project_id              text NOT NULL DEFAULT '',
  personal_model          boolean NOT NULL,
  agent_instance_id       text NOT NULL CHECK (length(agent_instance_id) BETWEEN 1 AND 128),
  agent_generation        bigint NOT NULL CHECK (agent_generation > 0),
  agent_session_id        text NOT NULL CHECK (length(agent_session_id) BETWEEN 1 AND 128),
  agent_root_session_id   text NOT NULL CHECK (length(agent_root_session_id) BETWEEN 1 AND 128),
  lease_owner             text NOT NULL CHECK (length(lease_owner) BETWEEN 1 AND 255),
  lease_version           bigint NOT NULL CHECK (lease_version > 0),
  authorization_version  bigint NOT NULL CHECK (authorization_version > 0),
  through_sequence        bigint NOT NULL CHECK (through_sequence >= 0),
  engine_version          text NOT NULL CHECK (length(engine_version) BETWEEN 1 AND 128),
  profile_version         text NOT NULL CHECK (length(profile_version) BETWEEN 1 AND 128),
  model_id                text NOT NULL,
  provider_model          text NOT NULL CHECK (length(provider_model) BETWEEN 1 AND 256),
  connection_version      timestamptz NOT NULL,
  request_digest          bytea NOT NULL CHECK (octet_length(request_digest) = 32),
  response_ciphertext     bytea NOT NULL CHECK (octet_length(response_ciphertext) BETWEEN 1 AND 8388608),
  response_digest         bytea NOT NULL CHECK (octet_length(response_digest) = 32),
  created_at              timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (owner_id, source, call_id),
  FOREIGN KEY (owner_id, source, call_id, task_id)
    REFERENCES model_call_ledger(owner_id, source, call_id, task_id) ON DELETE CASCADE,
  FOREIGN KEY (task_id, task_owner_id, initiator_id)
    REFERENCES tasks(id, owner_id, initiator_id) ON DELETE RESTRICT,
  FOREIGN KEY (task_id, agent_generation)
    REFERENCES task_agent_instances(task_id, agent_generation) ON DELETE RESTRICT,
  FOREIGN KEY (task_id, agent_session_id)
    REFERENCES task_agent_sessions(task_id, session_id) ON DELETE RESTRICT,
  FOREIGN KEY (task_id, agent_root_session_id)
    REFERENCES task_agent_sessions(task_id, session_id) ON DELETE RESTRICT
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM task_model_call_responses) THEN
    RAISE EXCEPTION 'cannot remove persisted Task model responses';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE task_model_call_responses;
