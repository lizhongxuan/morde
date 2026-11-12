-- +goose Up
ALTER TABLE task_hub_operations
  DROP CONSTRAINT task_hub_operations_task_revision_check,
  ADD CONSTRAINT task_hub_operations_task_revision_check
    CHECK ((action = 'file.fetch' AND task_revision >= 0) OR (action <> 'file.fetch' AND task_revision = 0));

ALTER TABLE task_file_fetches
  DROP CONSTRAINT task_file_fetches_task_revision_check,
  ADD CONSTRAINT task_file_fetches_task_revision_check CHECK (task_revision >= 0);

-- +goose Down
ALTER TABLE task_file_fetches
  DROP CONSTRAINT task_file_fetches_task_revision_check,
  ADD CONSTRAINT task_file_fetches_task_revision_check CHECK (task_revision > 0);

ALTER TABLE task_hub_operations
  DROP CONSTRAINT task_hub_operations_task_revision_check,
  ADD CONSTRAINT task_hub_operations_task_revision_check
    CHECK ((action = 'file.fetch' AND task_revision > 0) OR (action <> 'file.fetch' AND task_revision = 0));
