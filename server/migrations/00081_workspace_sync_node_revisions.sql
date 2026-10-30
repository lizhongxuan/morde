-- +goose Up
CREATE TABLE workspace_sync_node_revisions (
  owner_id text PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  desired_revision bigint NOT NULL CHECK (desired_revision > 0),
  applied_revision bigint NOT NULL DEFAULT 0 CHECK (applied_revision >= 0 AND applied_revision <= desired_revision),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX workspace_sync_node_revisions_pending
  ON workspace_sync_node_revisions(owner_id) WHERE desired_revision > applied_revision;

INSERT INTO workspace_sync_node_revisions(owner_id,desired_revision,applied_revision)
SELECT DISTINCT owner_id,1,0 FROM workspace_sync_folders
ON CONFLICT (owner_id) DO NOTHING;

ALTER TABLE workspace_sync_folders
  DROP CONSTRAINT workspace_sync_folders_state_check,
  ADD CONSTRAINT workspace_sync_folders_state_check
    CHECK (state IN ('pending_node', 'active', 'revoking', 'revoked'));

ALTER TABLE workspace_sync_device_bindings
  DROP CONSTRAINT workspace_sync_device_bindings_state_check,
  ADD CONSTRAINT workspace_sync_device_bindings_state_check
    CHECK (state IN ('pending_node', 'active', 'revoking', 'revoked'));

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM workspace_sync_node_revisions WHERE desired_revision > applied_revision)
     OR EXISTS (SELECT 1 FROM workspace_sync_folders WHERE state='revoking')
     OR EXISTS (SELECT 1 FROM workspace_sync_device_bindings WHERE state='revoking') THEN
    RAISE EXCEPTION 'cannot roll back Workspace sync node revisions while reconciliation is pending';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE workspace_sync_device_bindings
  DROP CONSTRAINT workspace_sync_device_bindings_state_check,
  ADD CONSTRAINT workspace_sync_device_bindings_state_check
    CHECK (state IN ('pending_node', 'active', 'revoked'));

ALTER TABLE workspace_sync_folders
  DROP CONSTRAINT workspace_sync_folders_state_check,
  ADD CONSTRAINT workspace_sync_folders_state_check
    CHECK (state IN ('pending_node', 'active', 'revoked'));

DROP TABLE workspace_sync_node_revisions;
