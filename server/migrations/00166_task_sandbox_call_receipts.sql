-- +goose Up
CREATE TABLE task_sandbox_call_receipts (
  id                       text PRIMARY KEY CHECK (id ~ '^o[a-z0-9]{7}$'),
  task_id                  text NOT NULL,
  owner_id                 text NOT NULL,
  initiator_id             text NOT NULL,
  project_id               text NOT NULL DEFAULT '',
  project_resource_version bigint NOT NULL DEFAULT 0 CHECK (project_resource_version >= 0),
  workspace_id             text NOT NULL,
  workspace_owner_id       text NOT NULL,
  input_revision           bigint NOT NULL CHECK (input_revision >= 0),
  workspace_checkpoint     bigint NOT NULL CHECK (workspace_checkpoint >= input_revision),
  context_digest           bytea NOT NULL CHECK (octet_length(context_digest) = 32),
  authorization_version    bigint NOT NULL CHECK (authorization_version > 0),
  lease_owner              text NOT NULL CHECK (length(lease_owner) BETWEEN 1 AND 255),
  lease_version            bigint NOT NULL CHECK (lease_version > 0),
  call_id                  text NOT NULL CHECK (length(call_id) BETWEEN 1 AND 256),
  request_hash             bytea NOT NULL CHECK (octet_length(request_hash) = 32),
  sandbox_id               text NOT NULL DEFAULT '' CHECK (length(sandbox_id) <= 512),
  sandbox_generation       bigint NOT NULL DEFAULT 0 CHECK (sandbox_generation >= 0),
  status                   text NOT NULL CHECK (status IN ('prepared', 'dispatched', 'succeeded', 'failed', 'unknown')),
  exit_code                integer,
  output                   text NOT NULL DEFAULT '' CHECK (octet_length(output) <= 65536),
  result_sha256            bytea CHECK (result_sha256 IS NULL OR octet_length(result_sha256) = 32),
  unknown_code             text NOT NULL DEFAULT '' CHECK (length(unknown_code) <= 64),
  created_at               timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at               timestamptz NOT NULL DEFAULT clock_timestamp(),
  UNIQUE (task_id, call_id),
  FOREIGN KEY (task_id, owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE,
  FOREIGN KEY (workspace_id, input_revision) REFERENCES workspace_revisions(workspace_id, revision),
  FOREIGN KEY (workspace_id, workspace_checkpoint) REFERENCES workspace_revisions(workspace_id, revision),
  CHECK ((project_id = '' AND project_resource_version = 0) OR (project_id <> '' AND project_resource_version > 0)),
  CHECK (
    (status IN ('dispatched', 'succeeded', 'failed', 'unknown') AND sandbox_id <> '' AND sandbox_generation > 0)
    OR (status = 'prepared' AND sandbox_id = '' AND sandbox_generation = 0)
  ),
  CHECK (
    (status IN ('succeeded', 'failed') AND exit_code IS NOT NULL AND result_sha256 IS NOT NULL AND unknown_code = '')
    OR (status IN ('prepared', 'dispatched') AND exit_code IS NULL AND result_sha256 IS NULL AND output = '' AND unknown_code = '')
    OR (status = 'unknown' AND exit_code IS NULL AND result_sha256 IS NULL AND output = '' AND unknown_code <> '')
  )
);

-- At most one sandbox call may be prepared, in flight, or unresolved for a
-- Task. This makes tool execution sequential until a dispatch is reconciled.
CREATE UNIQUE INDEX task_sandbox_call_receipts_one_active_idx
  ON task_sandbox_call_receipts (task_id)
  WHERE status IN ('prepared', 'dispatched', 'unknown');

-- +goose Down
DROP TABLE task_sandbox_call_receipts;
