-- +goose Up
ALTER TABLE workspace_revision_files
  ADD COLUMN xattrs jsonb NOT NULL DEFAULT '[]'::jsonb,
  ADD COLUMN acl jsonb,
  ADD COLUMN metadata_known boolean NOT NULL DEFAULT false,
  ADD CONSTRAINT workspace_revision_files_xattrs_check CHECK (jsonb_typeof(xattrs) = 'array'),
  ADD CONSTRAINT workspace_revision_files_acl_check CHECK (acl IS NULL OR jsonb_typeof(acl) = 'object');

ALTER TABLE workspace_revision_directories
  DROP CONSTRAINT workspace_revision_directories_directory_path_check,
  ADD CONSTRAINT workspace_revision_directories_directory_path_check CHECK (
    directory_path = '.' OR (directory_path <> '' AND octet_length(directory_path) <= 4096)
  ),
  ADD COLUMN xattrs jsonb NOT NULL DEFAULT '[]'::jsonb,
  ADD COLUMN acl jsonb,
  ADD COLUMN metadata_known boolean NOT NULL DEFAULT false,
  ADD CONSTRAINT workspace_revision_directories_xattrs_check CHECK (jsonb_typeof(xattrs) = 'array'),
  ADD CONSTRAINT workspace_revision_directories_acl_check CHECK (acl IS NULL OR jsonb_typeof(acl) = 'object');

ALTER TABLE workspace_changes
  ADD COLUMN directory jsonb,
  ADD CONSTRAINT workspace_changes_directory_check CHECK (
    directory IS NULL OR (action = 'directory_upsert' AND jsonb_typeof(directory) = 'object')
  );

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM workspace_revision_files WHERE xattrs <> '[]'::jsonb OR acl IS NOT NULL OR metadata_known) OR
     EXISTS (SELECT 1 FROM workspace_revision_directories WHERE directory_path='.' OR xattrs <> '[]'::jsonb OR acl IS NOT NULL OR metadata_known) OR
     EXISTS (SELECT 1 FROM workspace_changes WHERE directory IS NOT NULL OR file ? 'xattrs' OR file ? 'acl' OR file->'metadata_known' = 'true'::jsonb) OR
     EXISTS (SELECT 1 FROM workspace_sync_sessions s WHERE
       EXISTS (SELECT 1 FROM jsonb_array_elements(s.files) AS f(value) WHERE f.value ? 'xattrs' OR f.value ? 'acl' OR f.value->'metadata_known' = 'true'::jsonb) OR
       EXISTS (SELECT 1 FROM jsonb_array_elements(s.upload_files) AS f(value) WHERE f.value ? 'xattrs' OR f.value ? 'acl' OR f.value->'metadata_known' = 'true'::jsonb) OR
       EXISTS (SELECT 1 FROM jsonb_array_elements(s.download_files) AS f(value) WHERE f.value ? 'xattrs' OR f.value ? 'acl' OR f.value->'metadata_known' = 'true'::jsonb)) OR
     EXISTS (SELECT 1 FROM workspace_sync_session_directories d WHERE
       EXISTS (SELECT 1 FROM jsonb_array_elements(d.directories) AS entry(value) WHERE entry.value ? 'xattrs' OR entry.value ? 'acl' OR entry.value->'metadata_known' = 'true'::jsonb) OR
       EXISTS (SELECT 1 FROM jsonb_array_elements(d.upload_directories) AS entry(value) WHERE entry.value ? 'xattrs' OR entry.value ? 'acl' OR entry.value->'metadata_known' = 'true'::jsonb) OR
       EXISTS (SELECT 1 FROM jsonb_array_elements(d.download_directories) AS entry(value) WHERE entry.value ? 'xattrs' OR entry.value ? 'acl' OR entry.value->'metadata_known' = 'true'::jsonb)) THEN
    RAISE EXCEPTION 'cannot roll back Workspace native metadata after it has been written';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE workspace_changes
  DROP CONSTRAINT workspace_changes_directory_check,
  DROP COLUMN directory;

ALTER TABLE workspace_revision_directories
  DROP CONSTRAINT workspace_revision_directories_acl_check,
  DROP CONSTRAINT workspace_revision_directories_xattrs_check,
  DROP COLUMN acl,
  DROP COLUMN xattrs,
  DROP COLUMN metadata_known,
  DROP CONSTRAINT workspace_revision_directories_directory_path_check,
  ADD CONSTRAINT workspace_revision_directories_directory_path_check CHECK (
    directory_path <> '' AND octet_length(directory_path) <= 4096
  );

ALTER TABLE workspace_revision_files
  DROP CONSTRAINT workspace_revision_files_acl_check,
  DROP CONSTRAINT workspace_revision_files_xattrs_check,
  DROP COLUMN acl,
  DROP COLUMN xattrs,
  DROP COLUMN metadata_known;
