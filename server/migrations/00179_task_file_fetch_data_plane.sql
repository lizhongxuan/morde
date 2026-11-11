-- +goose Up
ALTER TABLE task_file_fetches
  ADD COLUMN media_type text,
  ADD COLUMN staging_expires_at timestamptz,
  ADD COLUMN content_state text NOT NULL DEFAULT 'none',
  ADD COLUMN parse_status text NOT NULL DEFAULT 'pending',
  ADD COLUMN parse_error text,
  ADD COLUMN deleted_at timestamptz,
  ADD CONSTRAINT task_file_fetches_identity_owner_task_unique UNIQUE (owner_id, task_id, fetch_id),
  ADD CONSTRAINT task_file_fetches_media_manifest_check
    CHECK ((source_version IS NULL) = (media_type IS NULL)),
  ADD CONSTRAINT task_file_fetches_staging_expiry_check
    CHECK ((status IN ('transferring', 'verifying')) = (staging_expires_at IS NOT NULL)),
  ADD CONSTRAINT task_file_fetches_content_state_check
    CHECK (content_state IN ('none', 'retained', 'deleted', 'expired')),
  ADD CONSTRAINT task_file_fetches_parse_status_check
    CHECK (parse_status IN ('pending', 'extracted', 'raw_only', 'failed', 'unavailable')),
  ADD CONSTRAINT task_file_fetches_parse_error_check
    CHECK ((parse_status IN ('failed', 'unavailable')) = (parse_error IS NOT NULL)),
  ADD CONSTRAINT task_file_fetches_parse_completion_check
    CHECK (status = 'completed' OR parse_status = 'pending'),
  ADD CONSTRAINT task_file_fetches_content_identity_check
    CHECK ((content_state = 'none') = (content_id IS NULL)),
  ADD CONSTRAINT task_file_fetches_content_retention_check
    CHECK (content_state = 'none' OR (status = 'completed' AND expires_at IS NOT NULL)),
  ADD CONSTRAINT task_file_fetches_deleted_at_check
    CHECK ((content_state = 'deleted') = (deleted_at IS NOT NULL));

CREATE TABLE task_file_fetch_chunks (
  owner_id       text NOT NULL,
  task_id        text NOT NULL,
  fetch_id       text NOT NULL,
  sequence       integer NOT NULL CHECK (sequence BETWEEN 0 AND 31),
  content        bytea NOT NULL CHECK (octet_length(content) BETWEEN 1 AND 524288),
  content_sha256 bytea NOT NULL CHECK (octet_length(content_sha256) = 32),
  created_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (owner_id, task_id, fetch_id, sequence),
  FOREIGN KEY (owner_id, task_id, fetch_id)
    REFERENCES task_file_fetches(owner_id, task_id, fetch_id) ON DELETE CASCADE
);

CREATE INDEX task_file_fetches_staging_expiry_idx
  ON task_file_fetches (staging_expires_at)
  WHERE staging_expires_at IS NOT NULL;
CREATE INDEX task_file_fetches_retained_expiry_idx
  ON task_file_fetches (expires_at)
  WHERE content_state = 'retained';

-- +goose Down
DROP TABLE task_file_fetch_chunks;
DROP INDEX task_file_fetches_retained_expiry_idx;
DROP INDEX task_file_fetches_staging_expiry_idx;
ALTER TABLE task_file_fetches
  DROP CONSTRAINT task_file_fetches_deleted_at_check,
  DROP CONSTRAINT task_file_fetches_parse_completion_check,
  DROP CONSTRAINT task_file_fetches_parse_error_check,
  DROP CONSTRAINT task_file_fetches_parse_status_check,
  DROP CONSTRAINT task_file_fetches_content_retention_check,
  DROP CONSTRAINT task_file_fetches_content_identity_check,
  DROP CONSTRAINT task_file_fetches_content_state_check,
  DROP CONSTRAINT task_file_fetches_staging_expiry_check,
  DROP CONSTRAINT task_file_fetches_media_manifest_check,
  DROP CONSTRAINT task_file_fetches_identity_owner_task_unique,
  DROP COLUMN deleted_at,
  DROP COLUMN parse_error,
  DROP COLUMN parse_status,
  DROP COLUMN content_state,
  DROP COLUMN staging_expires_at,
  DROP COLUMN media_type;
