-- +goose Up
ALTER TABLE workspace_revision_files
  ADD COLUMN kind text NOT NULL DEFAULT 'file',
  ADD COLUMN symlink_target text;

ALTER TABLE workspace_revision_files
  ADD CONSTRAINT workspace_revision_files_kind_target_check CHECK (
    (kind = 'file' AND symlink_target IS NULL) OR
    (kind = 'symlink' AND symlink_target IS NOT NULL AND symlink_target <> '' AND
      octet_length(symlink_target) <= 4096 AND left(symlink_target, 1) <> '/' AND
      strpos(symlink_target, ':') = 0 AND strpos(symlink_target, chr(92)) = 0 AND
      size_bytes = octet_length(symlink_target) AND mode = 0 AND chunks = '[]'::jsonb)
  );

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM workspace_revision_files WHERE kind = 'symlink') OR
     EXISTS (SELECT 1 FROM workspace_changes WHERE file @> '{"kind":"symlink"}'::jsonb) OR
     EXISTS (SELECT 1 FROM workspace_sync_sessions WHERE files @> '[{"kind":"symlink"}]'::jsonb OR
       upload_files @> '[{"kind":"symlink"}]'::jsonb OR download_files @> '[{"kind":"symlink"}]'::jsonb) THEN
    RAISE EXCEPTION 'cannot roll back Workspace symlink identity after it has been written';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE workspace_revision_files DROP CONSTRAINT workspace_revision_files_kind_target_check;
ALTER TABLE workspace_revision_files DROP COLUMN symlink_target;
ALTER TABLE workspace_revision_files DROP COLUMN kind;
