-- +goose Up
CREATE TABLE workspace_ownership_transfers (
  id text PRIMARY KEY CHECK (id ~ '^r[a-z0-9]{7}$'),
  workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  from_owner_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  to_owner_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  state text NOT NULL CHECK (state IN ('pending', 'accepting', 'unknown', 'accepted', 'cancelled')),
  expected_revision bigint NOT NULL CHECK (expected_revision >= 0),
  operation_id text CHECK (operation_id IS NULL OR operation_id ~ '^o[a-z0-9]{7}$'),
  version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  accepted_at timestamptz,
  UNIQUE (id, workspace_id),
  CHECK (from_owner_id <> to_owner_id),
  CHECK (
    (state IN ('pending', 'cancelled') AND operation_id IS NULL AND accepted_at IS NULL)
    OR (state IN ('accepting', 'unknown') AND operation_id IS NOT NULL AND accepted_at IS NULL)
    OR (state = 'accepted' AND operation_id IS NOT NULL AND accepted_at IS NOT NULL)
  )
);
CREATE UNIQUE INDEX workspace_ownership_transfers_one_frozen
  ON workspace_ownership_transfers(workspace_id)
  WHERE state IN ('pending', 'accepting', 'unknown');

ALTER TABLE workspace_volumes ADD COLUMN provisioned_owner_id text;
UPDATE workspace_volumes SET provisioned_owner_id=owner_id;
ALTER TABLE workspace_volumes
  ALTER COLUMN provisioned_owner_id SET NOT NULL,
  ADD CONSTRAINT workspace_volumes_provisioned_owner_fk
    FOREIGN KEY (provisioned_owner_id) REFERENCES users(id) ON DELETE RESTRICT;
ALTER TABLE workspace_volumes DROP CONSTRAINT workspace_volumes_workspace_owner_fk;
ALTER TABLE workspace_volumes ADD CONSTRAINT workspace_volumes_workspace_owner_fk
  FOREIGN KEY (workspace_id, owner_id) REFERENCES workspaces(id, owner_id)
  DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE workspace_ownership_history (
  workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  transfer_id text,
  started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  ended_at timestamptz,
  PRIMARY KEY (workspace_id, owner_id),
  FOREIGN KEY (transfer_id, workspace_id)
    REFERENCES workspace_ownership_transfers(id, workspace_id) ON DELETE CASCADE,
  CHECK (ended_at IS NULL OR ended_at > started_at)
);
CREATE UNIQUE INDEX workspace_ownership_history_one_current
  ON workspace_ownership_history(workspace_id) WHERE ended_at IS NULL;

INSERT INTO workspace_ownership_history(workspace_id, owner_id)
SELECT id, owner_id FROM workspaces;

-- Task and sync history remains attached to the owner at the time it was created.
ALTER TABLE tasks DROP CONSTRAINT tasks_workspace_owner_fkey;
ALTER TABLE tasks ADD CONSTRAINT tasks_workspace_owner_history_fkey
  FOREIGN KEY (workspace_id, owner_id) REFERENCES workspace_ownership_history(workspace_id, owner_id);

ALTER TABLE task_hub_operations DROP CONSTRAINT task_hub_operations_workspace_owner_fkey;
ALTER TABLE task_hub_operations ADD CONSTRAINT task_hub_operations_workspace_owner_history_fkey
  FOREIGN KEY (workspace_id, owner_id) REFERENCES workspace_ownership_history(workspace_id, owner_id);

ALTER TABLE workspace_sync_sessions DROP CONSTRAINT workspace_sync_sessions_workspace_id_owner_id_fkey;
ALTER TABLE workspace_sync_sessions ADD CONSTRAINT workspace_sync_sessions_workspace_owner_history_fkey
  FOREIGN KEY (workspace_id, owner_id) REFERENCES workspace_ownership_history(workspace_id, owner_id) ON DELETE CASCADE;

ALTER TABLE workspace_sync_cursors DROP CONSTRAINT workspace_sync_cursors_workspace_id_owner_id_fkey;
ALTER TABLE workspace_sync_cursors ADD CONSTRAINT workspace_sync_cursors_workspace_owner_history_fkey
  FOREIGN KEY (workspace_id, owner_id) REFERENCES workspace_ownership_history(workspace_id, owner_id) ON DELETE CASCADE;

ALTER TABLE workspace_sync_receive_reservations DROP CONSTRAINT workspace_sync_receive_reservations_workspace_id_owner_id_fkey;
ALTER TABLE workspace_sync_receive_reservations ADD CONSTRAINT workspace_sync_receive_reservations_workspace_owner_history_fkey
  FOREIGN KEY (workspace_id, owner_id) REFERENCES workspace_ownership_history(workspace_id, owner_id);

ALTER TABLE workspace_sync_receive_quota_rejections DROP CONSTRAINT workspace_sync_receive_quota_rejecti_workspace_id_owner_id_fkey;
ALTER TABLE workspace_sync_receive_quota_rejections ADD CONSTRAINT workspace_sync_receive_quota_rejections_workspace_owner_history_fkey
  FOREIGN KEY (workspace_id, owner_id) REFERENCES workspace_ownership_history(workspace_id, owner_id) ON DELETE CASCADE;

ALTER TABLE workspace_bindings DROP CONSTRAINT workspace_bindings_workspace_owner_fk;
ALTER TABLE workspace_bindings ADD CONSTRAINT workspace_bindings_workspace_owner_history_fk
  FOREIGN KEY (workspace_id, owner_id) REFERENCES workspace_ownership_history(workspace_id, owner_id);

ALTER TABLE project_resources DROP CONSTRAINT project_resources_workspace_id_resource_owner_id_fkey;
ALTER TABLE project_resources ADD CONSTRAINT project_resources_workspace_owner_history_fkey
  FOREIGN KEY (workspace_id, resource_owner_id) REFERENCES workspace_ownership_history(workspace_id, owner_id) ON DELETE RESTRICT;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM workspace_ownership_transfers)
     OR EXISTS (SELECT 1 FROM workspace_ownership_history WHERE ended_at IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot roll back Workspace ownership transfers after request or transfer history exists';
  END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE workspace_bindings DROP CONSTRAINT workspace_bindings_workspace_owner_history_fk;
ALTER TABLE workspace_bindings ADD CONSTRAINT workspace_bindings_workspace_owner_fk
  FOREIGN KEY (workspace_id, owner_id) REFERENCES workspaces(id, owner_id);

ALTER TABLE project_resources DROP CONSTRAINT project_resources_workspace_owner_history_fkey;
ALTER TABLE project_resources ADD CONSTRAINT project_resources_workspace_id_resource_owner_id_fkey
  FOREIGN KEY (workspace_id, resource_owner_id) REFERENCES workspaces(id, owner_id) ON DELETE RESTRICT;

ALTER TABLE workspace_sync_receive_quota_rejections DROP CONSTRAINT workspace_sync_receive_quota_rejections_workspace_owner_history_fkey;
ALTER TABLE workspace_sync_receive_quota_rejections ADD CONSTRAINT workspace_sync_receive_quota_rejecti_workspace_id_owner_id_fkey
  FOREIGN KEY (workspace_id, owner_id) REFERENCES workspaces(id, owner_id) ON DELETE CASCADE;

ALTER TABLE workspace_sync_receive_reservations DROP CONSTRAINT workspace_sync_receive_reservations_workspace_owner_history_fkey;
ALTER TABLE workspace_sync_receive_reservations ADD CONSTRAINT workspace_sync_receive_reservations_workspace_id_owner_id_fkey
  FOREIGN KEY (workspace_id, owner_id) REFERENCES workspaces(id, owner_id);

ALTER TABLE workspace_sync_cursors DROP CONSTRAINT workspace_sync_cursors_workspace_owner_history_fkey;
ALTER TABLE workspace_sync_cursors ADD CONSTRAINT workspace_sync_cursors_workspace_id_owner_id_fkey
  FOREIGN KEY (workspace_id, owner_id) REFERENCES workspaces(id, owner_id) ON DELETE CASCADE;

ALTER TABLE workspace_sync_sessions DROP CONSTRAINT workspace_sync_sessions_workspace_owner_history_fkey;
ALTER TABLE workspace_sync_sessions ADD CONSTRAINT workspace_sync_sessions_workspace_id_owner_id_fkey
  FOREIGN KEY (workspace_id, owner_id) REFERENCES workspaces(id, owner_id) ON DELETE CASCADE;

ALTER TABLE task_hub_operations DROP CONSTRAINT task_hub_operations_workspace_owner_history_fkey;
ALTER TABLE task_hub_operations ADD CONSTRAINT task_hub_operations_workspace_owner_fkey
  FOREIGN KEY (workspace_id, owner_id) REFERENCES workspaces(id, owner_id);

ALTER TABLE tasks DROP CONSTRAINT tasks_workspace_owner_history_fkey;
ALTER TABLE tasks ADD CONSTRAINT tasks_workspace_owner_fkey
  FOREIGN KEY (workspace_id, owner_id) REFERENCES workspaces(id, owner_id);

DROP INDEX workspace_ownership_history_one_current;
DROP TABLE workspace_ownership_history;
DROP INDEX workspace_ownership_transfers_one_frozen;
DROP TABLE workspace_ownership_transfers;
ALTER TABLE workspace_volumes DROP CONSTRAINT workspace_volumes_workspace_owner_fk;
ALTER TABLE workspace_volumes ADD CONSTRAINT workspace_volumes_workspace_owner_fk
  FOREIGN KEY (workspace_id, owner_id) REFERENCES workspaces(id, owner_id);
ALTER TABLE workspace_volumes DROP CONSTRAINT workspace_volumes_provisioned_owner_fk, DROP COLUMN provisioned_owner_id;
