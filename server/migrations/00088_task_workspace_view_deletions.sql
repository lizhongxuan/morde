-- +goose Up
CREATE TABLE task_workspace_view_deletions (
  request_id text PRIMARY KEY CHECK (request_id ~ '^z[a-z0-9]{7}$'),
  task_id text NOT NULL UNIQUE REFERENCES tasks(id) ON DELETE RESTRICT,
  owner_id text NOT NULL REFERENCES users(id),
  host_id text NOT NULL REFERENCES hosts(id),
  status text NOT NULL CHECK (status IN ('pending', 'applied')),
  requested_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  applied_at timestamptz,
  applied_generation bigint,
  CHECK (
    (status = 'pending' AND applied_at IS NULL AND applied_generation IS NULL) OR
    (status = 'applied' AND applied_at IS NOT NULL AND applied_generation > 0)
  )
);

CREATE INDEX task_workspace_view_deletions_pending_host
  ON task_workspace_view_deletions(host_id, requested_at, request_id)
  WHERE status = 'pending';

-- +goose Down
DROP TABLE task_workspace_view_deletions;
