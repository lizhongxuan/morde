-- +goose Up
CREATE TABLE host_terminal_sessions (
  id                  bigserial PRIMARY KEY,
  host_id             text NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
  owner_id            text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  actor_id            text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  runtime_generation bigint NOT NULL CHECK (runtime_generation > 0),
  control_version    bigint NOT NULL CHECK (control_version >= 0),
  workspace_id        text REFERENCES workspaces(id) ON DELETE SET NULL,
  attempt_id          text NOT NULL CHECK (attempt_id ~ '^o[a-z0-9]{7}$'),
  aio_session_id      text,
  state               text NOT NULL CHECK (state IN ('creating', 'ready', 'unknown', 'interrupted', 'abandoned')),
  created_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
  CHECK ((state = 'ready' AND aio_session_id IS NOT NULL) OR (state IN ('creating', 'unknown', 'interrupted') AND aio_session_id IS NULL) OR state = 'abandoned'),
  UNIQUE (host_id, actor_id, attempt_id)
);

CREATE UNIQUE INDEX host_terminal_sessions_current
  ON host_terminal_sessions (host_id, actor_id)
  WHERE state <> 'abandoned';

-- +goose Down
DROP TABLE host_terminal_sessions;
