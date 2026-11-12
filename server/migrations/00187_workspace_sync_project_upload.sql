-- +goose Up
ALTER TABLE workspace_sync_folders
  ADD COLUMN last_upload_revision bigint,
  ADD CONSTRAINT workspace_sync_folder_last_upload_revision_fk
    FOREIGN KEY (workspace_id,last_upload_revision)
    REFERENCES workspace_revisions(workspace_id,revision);

ALTER TABLE workspace_sync_sessions
  ADD COLUMN folder_id text,
  ADD COLUMN folder_baseline_revision bigint,
  ADD CONSTRAINT workspace_sync_session_folder_check CHECK (
    (folder_id IS NULL AND folder_baseline_revision IS NULL) OR
    (folder_id IS NOT NULL AND direction='upload' AND policy='compare_merge' AND cardinality(scope)=1 AND
      task_id IS NULL AND task_hub_operation_key IS NULL AND
      (folder_baseline_revision IS NULL OR base_revision=folder_baseline_revision))
  ),
  ADD CONSTRAINT workspace_sync_session_folder_fk
    FOREIGN KEY (workspace_id,owner_id,folder_id)
    REFERENCES workspace_sync_folders(workspace_id,owner_id,folder_id),
  ADD CONSTRAINT workspace_sync_session_folder_baseline_revision_fk
    FOREIGN KEY (workspace_id,folder_baseline_revision)
    REFERENCES workspace_revisions(workspace_id,revision);

CREATE INDEX workspace_sync_sessions_project_folder
  ON workspace_sync_sessions(workspace_id,folder_id,state,created_at DESC)
  WHERE folder_id IS NOT NULL;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM workspace_sync_sessions WHERE folder_id IS NOT NULL)
     OR EXISTS (SELECT 1 FROM workspace_sync_folders WHERE last_upload_revision IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot roll back Workspace project upload sessions or baselines';
  END IF;
END $$;
-- +goose StatementEnd

DROP INDEX workspace_sync_sessions_project_folder;
ALTER TABLE workspace_sync_sessions
  DROP CONSTRAINT workspace_sync_session_folder_baseline_revision_fk,
  DROP CONSTRAINT workspace_sync_session_folder_fk,
  DROP CONSTRAINT workspace_sync_session_folder_check,
  DROP COLUMN folder_baseline_revision,
  DROP COLUMN folder_id;
ALTER TABLE workspace_sync_folders
  DROP CONSTRAINT workspace_sync_folder_last_upload_revision_fk,
  DROP COLUMN last_upload_revision;
