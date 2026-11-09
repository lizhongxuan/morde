-- +goose Up
ALTER TABLE personal_attachment_uploads
  ADD COLUMN orphaned_at timestamptz,
  ADD COLUMN content_deleted_at timestamptz;

ALTER TABLE personal_attachment_uploads
  ADD CONSTRAINT personal_attachment_content_deleted_state_check
  CHECK (content_deleted_at IS NULL OR (state = 'committed' AND orphaned_at IS NOT NULL));

CREATE INDEX personal_attachment_orphan_cleanup_idx
  ON personal_attachment_uploads (service_instance_id, orphaned_at, upload_id)
  WHERE state = 'committed' AND content_deleted_at IS NULL;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM personal_attachment_uploads WHERE content_deleted_at IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot roll back physical personal attachment cleanup after deleting content';
  END IF;
END $$;
-- +goose StatementEnd
DROP INDEX personal_attachment_orphan_cleanup_idx;
ALTER TABLE personal_attachment_uploads
  DROP CONSTRAINT personal_attachment_content_deleted_state_check,
  DROP COLUMN content_deleted_at,
  DROP COLUMN orphaned_at;
