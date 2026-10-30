-- +goose Up
ALTER TABLE tasks
  ADD COLUMN workspace_checkpoint_revision bigint;

UPDATE tasks
SET workspace_checkpoint_revision = input_revision
WHERE input_revision IS NOT NULL;

ALTER TABLE tasks
  ADD CONSTRAINT tasks_workspace_checkpoint_revision_pair_check CHECK (
    (workspace_id IS NULL) = (workspace_checkpoint_revision IS NULL)
  ),
  ADD CONSTRAINT tasks_workspace_checkpoint_revision_check CHECK (
    workspace_checkpoint_revision IS NULL OR workspace_checkpoint_revision >= input_revision
  ),
  ADD CONSTRAINT tasks_workspace_checkpoint_revision_fkey
    FOREIGN KEY (workspace_id, workspace_checkpoint_revision)
    REFERENCES workspace_revisions(workspace_id, revision);

ALTER TABLE task_operations
  ADD COLUMN workspace_checkpoint_revision bigint;

UPDATE task_operations
SET workspace_checkpoint_revision = input_revision
WHERE input_revision IS NOT NULL;

ALTER TABLE task_operations
  ADD CONSTRAINT task_operations_workspace_checkpoint_revision_pair_check CHECK (
    workspace_checkpoint_revision IS NULL OR (workspace_id <> '' AND input_revision IS NOT NULL)
  ),
  ADD CONSTRAINT task_operations_workspace_checkpoint_revision_check CHECK (
    workspace_checkpoint_revision IS NULL OR workspace_checkpoint_revision >= input_revision
  ),
  ADD CONSTRAINT task_operations_workspace_checkpoint_revision_fkey
    FOREIGN KEY (workspace_id, workspace_checkpoint_revision)
    REFERENCES workspace_revisions(workspace_id, revision);

-- +goose Down
ALTER TABLE task_operations
  DROP CONSTRAINT task_operations_workspace_checkpoint_revision_fkey,
  DROP CONSTRAINT task_operations_workspace_checkpoint_revision_check,
  DROP CONSTRAINT task_operations_workspace_checkpoint_revision_pair_check,
  DROP COLUMN workspace_checkpoint_revision;

ALTER TABLE tasks
  DROP CONSTRAINT tasks_workspace_checkpoint_revision_fkey,
  DROP CONSTRAINT tasks_workspace_checkpoint_revision_check,
  DROP CONSTRAINT tasks_workspace_checkpoint_revision_pair_check,
  DROP COLUMN workspace_checkpoint_revision;
