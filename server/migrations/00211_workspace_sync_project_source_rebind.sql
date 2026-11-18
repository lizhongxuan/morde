-- +goose Up
-- A PC project source belongs to the user's stable Workspace. Its content
-- baseline survives a cloud-computer replacement and may be reattached to the
-- current Host when that Host registers the same Workspace folder.
ALTER TABLE workspace_sync_project_sources
  DROP CONSTRAINT workspace_sync_project_source_baseline_application_fk;

-- +goose StatementBegin
DO $$
DECLARE constraint_name text;
BEGIN
  FOR constraint_name IN
    SELECT conname
    FROM pg_constraint
    WHERE conrelid = 'workspace_sync_project_upload_applications'::regclass
      AND confrelid = 'workspace_sync_project_sources'::regclass
      AND contype = 'f'
  LOOP
    EXECUTE format('ALTER TABLE workspace_sync_project_upload_applications DROP CONSTRAINT %I', constraint_name);
  END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM workspace_sync_project_sources)
     OR EXISTS (SELECT 1 FROM workspace_sync_project_upload_applications)
     OR EXISTS (SELECT 1 FROM workspace_sync_sessions WHERE source_binding_id IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot restore Host-specific project source binding constraints after rebinding';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE workspace_sync_project_upload_applications
  ADD CONSTRAINT workspace_sync_project_upload_source_binding_fk
    FOREIGN KEY (source_binding_id, owner_id, workspace_id, folder_id, host_id, source_binding_version)
    REFERENCES workspace_sync_project_sources(source_binding_id, owner_id, workspace_id, folder_id, host_id, binding_version)
    ON DELETE RESTRICT;

ALTER TABLE workspace_sync_project_sources
  ADD CONSTRAINT workspace_sync_project_source_baseline_application_fk
    FOREIGN KEY (source_binding_id, owner_id, workspace_id, folder_id, host_id, binding_version,
                 last_applied_session_id, last_applied_target_revision, last_applied_manifest_sha256)
    REFERENCES workspace_sync_project_upload_applications(source_binding_id, owner_id, workspace_id, folder_id,
      host_id, source_binding_version, session_id, target_revision, manifest_sha256) ON DELETE RESTRICT;
