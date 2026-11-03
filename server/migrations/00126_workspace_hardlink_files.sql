-- +goose Up
ALTER TABLE workspace_revision_files ADD COLUMN hardlink_to text;

ALTER TABLE workspace_revision_files
  ADD CONSTRAINT workspace_revision_files_hardlink_check CHECK (
    hardlink_to IS NULL OR
    (kind = 'file' AND symlink_target IS NULL AND hardlink_to <> '' AND
      octet_length(hardlink_to) <= 4096 AND left(hardlink_to, 1) <> '/' AND
      strpos(hardlink_to, ':') = 0 AND strpos(hardlink_to, chr(92)) = 0 AND
      hardlink_to !~ '(^|/)([.]{1,2}|)(/|$)')
  );

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM workspace_revision_files WHERE hardlink_to IS NOT NULL) OR
     EXISTS (SELECT 1 FROM workspace_changes WHERE file ? 'hardlink_to') OR
     EXISTS (SELECT 1 FROM workspace_sync_sessions s
       WHERE EXISTS (SELECT 1 FROM jsonb_array_elements(s.files) AS f(value) WHERE f.value ? 'hardlink_to') OR
         EXISTS (SELECT 1 FROM jsonb_array_elements(s.upload_files) AS f(value) WHERE f.value ? 'hardlink_to') OR
         EXISTS (SELECT 1 FROM jsonb_array_elements(s.download_files) AS f(value) WHERE f.value ? 'hardlink_to')) THEN
    RAISE EXCEPTION 'cannot roll back Workspace hardlink identity after it has been written';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE workspace_revision_files DROP CONSTRAINT workspace_revision_files_hardlink_check;
ALTER TABLE workspace_revision_files DROP COLUMN hardlink_to;
