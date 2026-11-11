-- +goose Up
ALTER TABLE task_hub_operations
  DROP CONSTRAINT task_hub_operations_action_check,
  ADD COLUMN task_revision bigint NOT NULL DEFAULT 0,
  ADD CONSTRAINT task_hub_operations_action_check
    CHECK (action IN ('screenshot', 'files', 'file.get', 'file.put', 'file.fetch', 'command')),
  ADD CONSTRAINT task_hub_operations_task_revision_check
    CHECK ((action = 'file.fetch' AND task_revision > 0) OR (action <> 'file.fetch' AND task_revision = 0));

CREATE TABLE task_file_fetches (
  fetch_id                     text PRIMARY KEY CHECK (fetch_id ~ '^o[a-z0-9]{7}$'),
  task_id                      text NOT NULL,
  owner_id                     text NOT NULL,
  operation_key                text NOT NULL CHECK (length(operation_key) BETWEEN 1 AND 512),
  source_installation_id       text NOT NULL,
  target_grant_version         bigint NOT NULL CHECK (target_grant_version > 0),
  task_authorization_version   bigint NOT NULL CHECK (task_authorization_version > 0),
  task_revision                bigint NOT NULL CHECK (task_revision > 0),
  relative_path                text NOT NULL CHECK (
    octet_length(relative_path) BETWEEN 1 AND 1024
    AND relative_path !~ '(^/|~|//|/$|^[A-Za-z]:|(^|/)([.]|[.][.])(/|$))'
    AND position(chr(92) IN relative_path) = 0
  ),
  request_hash                 bytea NOT NULL CHECK (octet_length(request_hash) = 32),
  status                       text NOT NULL CHECK (status IN (
    'queued', 'waiting_device', 'transferring', 'verifying', 'completed', 'cancelling', 'cancelled', 'failed'
  )),
  waiting_reason               text CHECK (waiting_reason IS NULL OR waiting_reason IN ('device_offline')),
  source_version               text CHECK (source_version IS NULL OR source_version ~ '^[a-f0-9]{64}$'),
  source_sha256                text CHECK (source_sha256 IS NULL OR source_sha256 ~ '^[a-f0-9]{64}$'),
  size_bytes                   bigint CHECK (size_bytes IS NULL OR size_bytes BETWEEN 0 AND 16777216),
  confirmed_bytes              bigint NOT NULL DEFAULT 0 CHECK (confirmed_bytes >= 0),
  content_id                   text CHECK (content_id IS NULL OR content_id ~ '^o[a-z0-9]{7}$'),
  ticket_hash                  bytea CHECK (ticket_hash IS NULL OR octet_length(ticket_hash) = 32),
  ticket_expires_at            timestamptz,
  expires_at                   timestamptz,
  created_at                   timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at                   timestamptz NOT NULL DEFAULT clock_timestamp(),
  UNIQUE (owner_id, task_id, operation_key),
  FOREIGN KEY (task_id, owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE,
  FOREIGN KEY (source_installation_id, owner_id) REFERENCES client_installations(id, owner_id),
  FOREIGN KEY (owner_id, task_id, operation_key)
    REFERENCES task_hub_operations(owner_id, task_id, operation_key) ON DELETE CASCADE,
  CHECK ((ticket_hash IS NULL) = (ticket_expires_at IS NULL)),
  CHECK ((source_version IS NULL) = (source_sha256 IS NULL)),
  CHECK ((source_version IS NULL) = (size_bytes IS NULL)),
  CHECK (size_bytes IS NULL OR confirmed_bytes <= size_bytes),
  CHECK ((content_id IS NULL) = (expires_at IS NULL)),
  CHECK ((status = 'waiting_device') = (waiting_reason IS NOT NULL))
);

CREATE INDEX task_file_fetches_task_status_idx
  ON task_file_fetches (owner_id, task_id, status, created_at DESC);
CREATE INDEX task_file_fetches_ticket_expiry_idx
  ON task_file_fetches (ticket_expires_at)
  WHERE ticket_hash IS NOT NULL;
CREATE INDEX task_file_fetches_content_expiry_idx
  ON task_file_fetches (expires_at)
  WHERE status = 'completed';

-- +goose Down
DROP TABLE task_file_fetches;
ALTER TABLE task_hub_operations
  DROP CONSTRAINT task_hub_operations_task_revision_check,
  DROP CONSTRAINT task_hub_operations_action_check,
  DROP COLUMN task_revision,
  ADD CONSTRAINT task_hub_operations_action_check
    CHECK (action IN ('screenshot', 'files', 'file.get', 'file.put', 'command'));
