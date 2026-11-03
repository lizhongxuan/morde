-- +goose Up
CREATE TABLE task_workbench_observation_tickets (
  ticket_hash        bytea PRIMARY KEY CHECK (octet_length(ticket_hash) = 32),
  nonce_hash         bytea NOT NULL CHECK (octet_length(nonce_hash) = 32),
  session_hash       bytea UNIQUE CHECK (session_hash IS NULL OR octet_length(session_hash) = 32),
  task_id            text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  host_id            text NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
  actor_user_id      text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  session_token_id   text NOT NULL REFERENCES tokens(id) ON DELETE CASCADE,
  runtime_generation bigint NOT NULL CHECK (runtime_generation > 0),
  read_scope         text NOT NULL CHECK (read_scope = 'task:read'),
  expires_at         timestamptz NOT NULL,
  used_at            timestamptz,
  session_expires_at timestamptz,
  created_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
  CHECK (
    (used_at IS NULL AND session_hash IS NULL AND session_expires_at IS NULL) OR
    (used_at IS NOT NULL AND session_hash IS NOT NULL AND session_expires_at IS NOT NULL)
  )
);

CREATE INDEX task_workbench_observation_tickets_created_at
  ON task_workbench_observation_tickets(created_at);

-- +goose Down
DROP TABLE task_workbench_observation_tickets;
