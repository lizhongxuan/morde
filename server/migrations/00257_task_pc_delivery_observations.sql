-- +goose Up
CREATE TABLE task_pc_delivery_observations (
  task_id               text NOT NULL,
  owner_id              text NOT NULL,
  conversation_id       text NOT NULL,
  client_installation_id text NOT NULL REFERENCES client_installations(id) ON DELETE CASCADE,
  message_id            text NOT NULL REFERENCES conversation_messages(id) ON DELETE CASCADE,
  task_revision         bigint NOT NULL CHECK (task_revision >= 0),
  observed_at           timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (task_id, owner_id, client_installation_id, message_id, task_revision),
  FOREIGN KEY (task_id, owner_id, conversation_id) REFERENCES tasks(id, owner_id, conversation_id) ON DELETE CASCADE
);

CREATE INDEX task_pc_delivery_observations_task
  ON task_pc_delivery_observations(task_id, owner_id, conversation_id, observed_at, message_id);

-- +goose Down
DROP TABLE task_pc_delivery_observations;
