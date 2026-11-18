-- +goose Up
ALTER TABLE task_workspace_publish_receipts
  ADD COLUMN direct_directory_source boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE task_workspace_publish_receipts
  DROP COLUMN direct_directory_source;
