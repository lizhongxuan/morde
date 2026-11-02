-- +goose Up
CREATE TABLE task_hub_operations (
  task_id                     text NOT NULL,
  owner_id                    text NOT NULL,
  operation_key               text NOT NULL CHECK (length(operation_key) BETWEEN 1 AND 512),
  tool_name                   text NOT NULL CHECK (length(tool_name) BETWEEN 1 AND 128),
  target_installation_id      text NOT NULL CHECK (target_installation_id ~ '^i[a-z0-9]{7}$'),
  action                      text NOT NULL CHECK (action IN ('screenshot', 'files', 'file.get', 'command')),
  task_authorization_version  bigint NOT NULL CHECK (task_authorization_version > 0),
  target_grant_version        bigint NOT NULL CHECK (target_grant_version > 0),
  lease_version               bigint NOT NULL CHECK (lease_version > 0),
  request_hash                bytea NOT NULL CHECK (octet_length(request_hash) = 32),
  result_hash                 bytea CHECK (result_hash IS NULL OR octet_length(result_hash) = 32),
  status                      text NOT NULL CHECK (status IN ('prepared', 'dispatched', 'succeeded', 'failed', 'unknown')),
  created_at                  timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at                  timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (owner_id, task_id, operation_key),
  FOREIGN KEY (task_id, owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE,
  FOREIGN KEY (target_installation_id, owner_id) REFERENCES client_installations(id, owner_id),
  CHECK (
    (status IN ('succeeded', 'failed') AND result_hash IS NOT NULL)
    OR (status IN ('prepared', 'dispatched', 'unknown') AND result_hash IS NULL)
  )
);

CREATE INDEX task_hub_operations_status_idx
  ON task_hub_operations (task_id, status)
  WHERE status IN ('prepared', 'dispatched', 'unknown');

-- +goose Down
DROP TABLE task_hub_operations;
