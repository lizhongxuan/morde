-- +goose Up
ALTER TABLE workspaces DROP CONSTRAINT workspaces_state_check;
ALTER TABLE workspaces ADD CONSTRAINT workspaces_state_check
  CHECK (state IN ('ready', 'copying', 'deleting', 'deleted'));

CREATE TABLE workspace_copy_reservations (
  id text PRIMARY KEY CHECK (id ~ '^r[a-z0-9]{7}$'),
  owner_id text NOT NULL REFERENCES users(id),
  source_workspace_id text NOT NULL,
  target_workspace_id text NOT NULL,
  service_instance_id text NOT NULL CHECK (length(service_instance_id) BETWEEN 1 AND 128),
  source_revision bigint NOT NULL CHECK (source_revision >= 0),
  reserved_bytes bigint NOT NULL DEFAULT 0 CHECK (reserved_bytes >= 0),
  state text NOT NULL CHECK (state IN ('reserved', 'copying', 'unknown', 'completed', 'failed')),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  CONSTRAINT workspace_copy_reservations_source_owner_fk
    FOREIGN KEY (source_workspace_id, owner_id) REFERENCES workspaces(id, owner_id),
  CONSTRAINT workspace_copy_reservations_target_fk
    FOREIGN KEY (target_workspace_id) REFERENCES workspaces(id),
  CHECK (source_workspace_id <> target_workspace_id),
  UNIQUE (target_workspace_id)
);

CREATE UNIQUE INDEX workspace_copy_reservations_source_active
  ON workspace_copy_reservations(source_workspace_id)
  WHERE state IN ('reserved', 'copying', 'unknown');
CREATE INDEX workspace_copy_reservations_target_active
  ON workspace_copy_reservations(target_workspace_id)
  WHERE state IN ('reserved', 'copying', 'unknown');

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM workspace_copy_reservations)
     OR EXISTS (SELECT 1 FROM workspaces WHERE state = 'copying') THEN
    RAISE EXCEPTION 'cannot roll back Workspace copy reservations after a copy has been recorded';
  END IF;
END $$;
-- +goose StatementEnd

DROP TABLE workspace_copy_reservations;
ALTER TABLE workspaces DROP CONSTRAINT workspaces_state_check;
ALTER TABLE workspaces ADD CONSTRAINT workspaces_state_check
  CHECK (state IN ('ready', 'deleting', 'deleted'));
