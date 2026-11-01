-- +goose Up
-- Keep quota pauses durable while allowing an identical retry to be re-evaluated
-- after capacity changes; the operation hash prevents a different request reusing its ID.
CREATE TABLE workspace_sync_receive_quota_rejections (
  owner_id text NOT NULL,
  workspace_id text NOT NULL,
  operation_id text NOT NULL CHECK (operation_id ~ '^[a-f0-9]{64}$'),
  request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
  file_path text NOT NULL CHECK (file_path <> '' AND octet_length(file_path) <= 4096),
  folded_path text NOT NULL,
  operation text NOT NULL CHECK (operation = 'upsert'),
  target_size bigint NOT NULL CHECK (target_size BETWEEN 0 AND 314572800),
  base_file_path text CHECK (base_file_path IS NULL OR (base_file_path <> '' AND octet_length(base_file_path) <= 4096)),
  base_file_size bigint CHECK (base_file_size >= 0),
  base_file_sha256 text CHECK (base_file_sha256 ~ '^[a-f0-9]{64}$'),
  base_file_mode integer CHECK (base_file_mode BETWEEN 0 AND 511),
  base_empty_directory_path text CHECK (base_empty_directory_path IS NULL OR (base_empty_directory_path <> '' AND octet_length(base_empty_directory_path) <= 4096)),
  reason_code text NOT NULL CHECK (reason_code IN ('workspace_quota_exceeded', 'owner_storage_quota_exceeded')),
  state text NOT NULL CHECK (state IN ('paused', 'resumed', 'superseded')),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (owner_id, workspace_id, operation_id),
  FOREIGN KEY (workspace_id, owner_id) REFERENCES workspaces(id, owner_id) ON DELETE CASCADE,
  CHECK ((base_file_path IS NULL) = (base_file_size IS NULL)),
  CHECK ((base_file_path IS NULL) = (base_file_sha256 IS NULL)),
  CHECK ((base_file_path IS NULL) = (base_file_mode IS NULL)),
  CHECK (NOT (base_file_path IS NOT NULL AND base_empty_directory_path IS NOT NULL))
);

CREATE INDEX workspace_sync_receive_quota_pauses_active
  ON workspace_sync_receive_quota_rejections(owner_id, workspace_id, folded_path, updated_at DESC, operation_id DESC)
  WHERE state='paused';

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM workspace_sync_receive_quota_rejections) THEN
    RAISE EXCEPTION 'cannot remove Workspace receive quota rejection history';
  END IF;
END $$;
-- +goose StatementEnd

DROP INDEX workspace_sync_receive_quota_pauses_active;
DROP TABLE workspace_sync_receive_quota_rejections;
