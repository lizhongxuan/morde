-- +goose Up
ALTER TABLE task_operations
  ADD COLUMN input_revision bigint;

UPDATE task_operations o
SET input_revision = t.input_revision
FROM tasks t
WHERE o.task_id = t.id AND o.workspace_id = t.workspace_id
  AND t.input_revision IS NOT NULL AND o.input_revision IS NULL;

ALTER TABLE task_operations
  ADD CONSTRAINT task_operations_input_revision_check CHECK (
    input_revision IS NULL OR (workspace_id <> '' AND input_revision >= 0)
  ),
  ADD CONSTRAINT task_operations_input_revision_fkey
    FOREIGN KEY (workspace_id, input_revision)
    REFERENCES workspace_revisions(workspace_id, revision) ON DELETE RESTRICT;

-- +goose Down
ALTER TABLE task_operations
  DROP CONSTRAINT task_operations_input_revision_fkey,
  DROP CONSTRAINT task_operations_input_revision_check,
  DROP COLUMN input_revision;
