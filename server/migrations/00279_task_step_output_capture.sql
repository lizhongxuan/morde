-- +goose Up
CREATE TABLE task_step_output_captures (
  task_id                         text NOT NULL,
  owner_id                        text NOT NULL,
  step_id                         text NOT NULL,
  operation_id                    text NOT NULL,
  call_id                         text NOT NULL CHECK (char_length(call_id) BETWEEN 1 AND 256),
  task_revision                   bigint NOT NULL CHECK (task_revision >= 0),
  authorization_version           bigint NOT NULL CHECK (authorization_version > 0),
  workspace_id                    text NOT NULL CHECK (workspace_id ~ '^w[a-z0-9]{7}$'),
  workspace_authorization_version bigint NOT NULL CHECK (workspace_authorization_version >= 0),
  workspace_binding_version       bigint NOT NULL CHECK (workspace_binding_version > 0),
  workspace_view_id               text NOT NULL CHECK (workspace_view_id ~ '^v[0-9a-f]{64}$'),
  workspace_checkpoint_revision   bigint NOT NULL CHECK (workspace_checkpoint_revision >= 0),
  stdout_source_bytes             integer NOT NULL CHECK (stdout_source_bytes BETWEEN 0 AND 2097152),
  stdout_total_bytes              integer NOT NULL CHECK (stdout_total_bytes BETWEEN 0 AND 2097152),
  stdout_sha256                   bytea NOT NULL CHECK (octet_length(stdout_sha256) = 32),
  stderr_source_bytes             integer NOT NULL CHECK (stderr_source_bytes BETWEEN 0 AND 2097152),
  stderr_total_bytes              integer NOT NULL CHECK (stderr_total_bytes BETWEEN 0 AND 2097152),
  stderr_sha256                   bytea NOT NULL CHECK (octet_length(stderr_sha256) = 32),
  stored_bytes                    integer NOT NULL CHECK (stored_bytes BETWEEN 0 AND 2097152),
  storage_bytes                   integer NOT NULL CHECK (storage_bytes BETWEEN 0 AND 2100736),
  page_count                      integer NOT NULL CHECK (page_count BETWEEN 0 AND 128),
  state                           text NOT NULL CHECK (state = 'complete'),
  created_at                      timestamptz NOT NULL DEFAULT clock_timestamp(),
  expires_at                      timestamptz NOT NULL,
  PRIMARY KEY (task_id, step_id),
  UNIQUE (task_id, operation_id),
  FOREIGN KEY (task_id, owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE,
  FOREIGN KEY (task_id, step_id) REFERENCES task_steps(task_id, id) ON DELETE CASCADE,
  FOREIGN KEY (task_id, operation_id) REFERENCES task_operations(task_id, id) ON DELETE CASCADE,
  CHECK (stdout_source_bytes + stderr_source_bytes <= 2097152),
  CHECK (stdout_total_bytes + stderr_total_bytes = stored_bytes),
  CHECK (storage_bytes >= stored_bytes),
  CHECK (expires_at > created_at AND expires_at <= created_at + interval '24 hours')
);

CREATE INDEX task_step_output_captures_owner_expiry
  ON task_step_output_captures(owner_id, expires_at);

CREATE TABLE task_step_output_capture_pages (
  task_id             text NOT NULL,
  step_id             text NOT NULL,
  stream              text NOT NULL CHECK (stream IN ('stdout','stderr')),
  page_index          integer NOT NULL CHECK (page_index BETWEEN 0 AND 127),
  page_offset         integer NOT NULL CHECK (page_offset >= 0),
  plaintext_bytes     integer NOT NULL CHECK (plaintext_bytes BETWEEN 1 AND 16384),
  page_sha256         bytea NOT NULL CHECK (octet_length(page_sha256) = 32),
  payload_ciphertext  bytea NOT NULL CHECK (octet_length(payload_ciphertext) = plaintext_bytes + 28),
  created_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (task_id, step_id, stream, page_offset),
  UNIQUE (task_id, step_id, stream, page_index),
  FOREIGN KEY (task_id, step_id) REFERENCES task_step_output_captures(task_id, step_id) ON DELETE CASCADE
);

CREATE INDEX task_step_output_capture_pages_cleanup
  ON task_step_output_capture_pages(task_id, step_id);

-- +goose Down
DROP TABLE task_step_output_capture_pages;
DROP TABLE task_step_output_captures;
