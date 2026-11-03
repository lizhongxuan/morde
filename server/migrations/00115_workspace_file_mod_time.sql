-- +goose Up
-- Historical revisions did not record file modification times; NULL keeps
-- that unknown state distinct from a real Unix epoch timestamp.
ALTER TABLE workspace_revision_files ADD COLUMN mod_time_ns bigint;
ALTER TABLE workspace_sync_receive_quota_rejections ADD COLUMN base_file_mod_time_ns bigint;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM workspace_revision_files WHERE mod_time_ns IS NOT NULL)
     OR EXISTS (SELECT 1 FROM workspace_sync_receive_quota_rejections WHERE base_file_mod_time_ns IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot roll back Workspace file modification times after revisions have recorded them';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE workspace_revision_files DROP COLUMN mod_time_ns;
ALTER TABLE workspace_sync_receive_quota_rejections DROP COLUMN base_file_mod_time_ns;
