-- +goose Up
ALTER TABLE workspace_bindings
  ADD COLUMN sync_state text NOT NULL DEFAULT 'healthy'
    CHECK (sync_state IN ('healthy', 'conflict')),
  ADD COLUMN sync_conflicts text[] NOT NULL DEFAULT '{}'::text[]
    CHECK (cardinality(sync_conflicts) <= 100);

-- +goose Down
ALTER TABLE workspace_bindings
  DROP COLUMN sync_conflicts,
  DROP COLUMN sync_state;
