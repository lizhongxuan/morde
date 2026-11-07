-- +goose Up
-- Keep the conversation owner and the Workspace's historical owner distinct.
-- Existing workspace references were constrained to the old Task owner, so
-- this backfill preserves the actual historical foreign-key identity.
ALTER TABLE tasks ADD COLUMN workspace_owner_id text;
ALTER TABLE tasks ADD COLUMN workspace_project_resource_version bigint;
UPDATE tasks SET workspace_owner_id=owner_id WHERE workspace_id IS NOT NULL;
ALTER TABLE tasks
  ADD CONSTRAINT tasks_workspace_owner_presence_check CHECK (
    (workspace_id IS NULL AND workspace_owner_id IS NULL AND workspace_project_resource_version IS NULL) OR
    (workspace_id IS NOT NULL AND workspace_owner_id IS NOT NULL AND workspace_owner_id <> '')
  ),
  ADD CONSTRAINT tasks_workspace_project_resource_version_check CHECK (
    workspace_project_resource_version IS NULL OR (project_id IS NOT NULL AND workspace_project_resource_version > 0)
  ),
  ADD CONSTRAINT tasks_workspace_resource_owner_history_fkey FOREIGN KEY (workspace_id,workspace_owner_id)
    REFERENCES workspace_ownership_history(workspace_id,owner_id);
ALTER TABLE tasks DROP CONSTRAINT tasks_workspace_owner_history_fkey;

-- A project member's access to somebody else's Workspace comes from one
-- specific Host membership and binding. Retain that identity rather than
-- letting a later revoke/regrant revive an older Task checkpoint.
CREATE TABLE task_workspace_grant_fences (
  task_id text PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
  project_id text NOT NULL,
  workspace_id text NOT NULL,
  workspace_owner_id text NOT NULL,
  actor_id text NOT NULL,
  host_id text NOT NULL,
  binding_version bigint NOT NULL CHECK (binding_version > 0),
  grant_version uuid NOT NULL,
  grant_fingerprint text NOT NULL CHECK (length(grant_fingerprint) = 64),
  CHECK (actor_id <> workspace_owner_id)
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM tasks WHERE workspace_id IS NOT NULL AND workspace_owner_id<>owner_id) THEN
    RAISE EXCEPTION 'cannot restore the old Task Workspace owner foreign key';
  END IF;
  IF EXISTS (SELECT 1 FROM task_workspace_grant_fences) THEN
    RAISE EXCEPTION 'cannot remove pinned project Task Workspace grants';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE task_workspace_grant_fences;
ALTER TABLE tasks ADD CONSTRAINT tasks_workspace_owner_history_fkey FOREIGN KEY (workspace_id,owner_id)
  REFERENCES workspace_ownership_history(workspace_id,owner_id);
ALTER TABLE tasks DROP CONSTRAINT tasks_workspace_resource_owner_history_fkey;
ALTER TABLE tasks DROP CONSTRAINT tasks_workspace_project_resource_version_check;
ALTER TABLE tasks DROP CONSTRAINT tasks_workspace_owner_presence_check;
ALTER TABLE tasks DROP COLUMN workspace_project_resource_version;
ALTER TABLE tasks DROP COLUMN workspace_owner_id;
