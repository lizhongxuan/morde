-- +goose Up
CREATE TABLE task_step_output_capture_statuses (
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
  state                           text NOT NULL CHECK (state IN ('source_truncated','capacity_truncated','failed')),
  created_at                      timestamptz NOT NULL DEFAULT clock_timestamp(),
  expires_at                      timestamptz NOT NULL,
  PRIMARY KEY (task_id, step_id),
  UNIQUE (task_id, operation_id),
  FOREIGN KEY (task_id, owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE,
  FOREIGN KEY (task_id, step_id) REFERENCES task_steps(task_id, id) ON DELETE CASCADE,
  FOREIGN KEY (task_id, operation_id) REFERENCES task_operations(task_id, id) ON DELETE CASCADE,
  CHECK (expires_at > created_at AND expires_at <= created_at + interval '24 hours')
);

CREATE INDEX task_step_output_capture_statuses_owner_expiry
  ON task_step_output_capture_statuses(owner_id, expires_at);

-- +goose Down
DROP TABLE task_step_output_capture_statuses;
