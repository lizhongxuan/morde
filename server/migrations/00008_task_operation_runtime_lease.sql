-- +goose Up
ALTER TABLE task_operations
  ADD COLUMN runtime_instance_id text NOT NULL DEFAULT '',
  ADD COLUMN runtime_lease_version bigint NOT NULL DEFAULT 0
    CHECK (runtime_lease_version >= 0);

-- Existing rows deliberately retain the zero/empty sentinel. Earlier versions
-- rewrote lease_version when a new worker claimed a task, so the original
-- runtime query fence cannot be reconstructed safely for those operations.

-- +goose Down
ALTER TABLE task_operations
  DROP COLUMN runtime_lease_version,
  DROP COLUMN runtime_instance_id;
