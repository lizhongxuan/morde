-- +goose Up
-- Empty leaf directories are first-class Workspace snapshot entries. Existing
-- revisions have no directory entries and remain valid without backfill.
ALTER TABLE workspace_revisions
  ADD COLUMN directory_count bigint NOT NULL DEFAULT 0 CHECK (directory_count >= 0);

CREATE TABLE workspace_revision_empty_directories (
  workspace_id text NOT NULL,
  valid_from_revision bigint NOT NULL,
  valid_to_revision bigint,
  directory_path text NOT NULL CHECK (directory_path <> '' AND octet_length(directory_path) <= 4096),
  folded_path text NOT NULL,
  PRIMARY KEY (workspace_id, directory_path, valid_from_revision),
  FOREIGN KEY (workspace_id, valid_from_revision) REFERENCES workspace_revisions(workspace_id, revision) ON DELETE CASCADE,
  FOREIGN KEY (workspace_id, valid_to_revision) REFERENCES workspace_revisions(workspace_id, revision) ON DELETE CASCADE,
  CHECK (valid_to_revision IS NULL OR valid_to_revision > valid_from_revision)
);
CREATE UNIQUE INDEX workspace_revision_empty_directories_active_casefold
  ON workspace_revision_empty_directories(workspace_id, folded_path)
  WHERE valid_to_revision IS NULL;
CREATE INDEX workspace_revision_empty_directories_page
  ON workspace_revision_empty_directories(workspace_id, directory_path, valid_from_revision, valid_to_revision);

ALTER TABLE workspace_changes DROP CONSTRAINT workspace_changes_action_check;
ALTER TABLE workspace_changes DROP CONSTRAINT workspace_changes_check;
ALTER TABLE workspace_changes
  ADD CONSTRAINT workspace_changes_action_check
    CHECK (action IN ('upsert', 'delete', 'directory_upsert', 'directory_delete')),
  ADD CONSTRAINT workspace_changes_check
    CHECK ((action = 'upsert' AND file IS NOT NULL) OR
           (action IN ('delete', 'directory_upsert', 'directory_delete') AND file IS NULL));

ALTER TABLE workspace_sync_sessions
  ADD COLUMN empty_directories text[] NOT NULL DEFAULT '{}',
  ADD COLUMN upload_empty_directories text[] NOT NULL DEFAULT '{}',
  ADD COLUMN download_empty_directories text[] NOT NULL DEFAULT '{}',
  ADD COLUMN deleted_directories text[] NOT NULL DEFAULT '{}',
  ADD COLUMN upload_deleted_directories text[] NOT NULL DEFAULT '{}',
  ADD COLUMN download_deleted_directories text[] NOT NULL DEFAULT '{}';

-- Direct Syncthing receive reservations share the same revision ledger as
-- explicit sync commits, so directory-only operations must be durable too.
ALTER TABLE workspace_sync_receive_reservations
  ADD COLUMN base_empty_directory_path text
    CHECK (base_empty_directory_path IS NULL OR
           (base_empty_directory_path <> '' AND octet_length(base_empty_directory_path) <= 4096));
ALTER TABLE workspace_sync_receive_reservations
  DROP CONSTRAINT workspace_sync_receive_reservations_operation_check,
  DROP CONSTRAINT workspace_sync_receive_reservations_check,
  DROP CONSTRAINT workspace_sync_receive_reservations_check1,
  DROP CONSTRAINT workspace_sync_receive_reservations_check2;
ALTER TABLE workspace_sync_receive_reservations
  ADD CONSTRAINT workspace_sync_receive_reservations_operation_check
    CHECK (operation IN ('upsert', 'delete', 'directory_upsert', 'directory_delete')),
  ADD CONSTRAINT workspace_sync_receive_reservations_check
    CHECK ((base_file_size IS NULL) = (base_file_sha256 IS NULL) AND
           (base_file_size IS NULL OR base_empty_directory_path IS NULL)),
  ADD CONSTRAINT workspace_sync_receive_reservations_check1
    CHECK ((operation = 'upsert') OR target_size = 0),
  ADD CONSTRAINT workspace_sync_receive_reservations_check2
    CHECK ((state IN ('reserved', 'aborting', 'aborted') AND result_revision IS NULL AND result_sha256 IS NULL AND result_mode IS NULL) OR
           (state = 'settled' AND result_revision IS NOT NULL AND
            ((operation = 'upsert' AND result_sha256 IS NOT NULL AND result_mode IS NOT NULL) OR
             (operation IN ('delete', 'directory_upsert', 'directory_delete') AND result_sha256 IS NULL AND result_mode IS NULL))));

ALTER TABLE workspace_sync_session_download_actions
  DROP CONSTRAINT workspace_sync_session_download_actions_action_check,
  DROP CONSTRAINT workspace_sync_session_download_actions_check;
ALTER TABLE workspace_sync_session_download_actions
  ADD CONSTRAINT workspace_sync_session_download_actions_action_check
    CHECK (action IN ('upsert', 'delete', 'directory_upsert', 'directory_delete')),
  ADD CONSTRAINT workspace_sync_session_download_actions_check
    CHECK ((action = 'upsert' AND sha256 ~ '^[a-f0-9]{64}$') OR
           (action IN ('delete', 'directory_upsert', 'directory_delete') AND sha256 IS NULL AND size_bytes = 0));

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM workspace_revision_empty_directories)
     OR EXISTS (SELECT 1 FROM workspace_sync_receive_reservations
                WHERE operation IN ('directory_upsert', 'directory_delete') OR base_empty_directory_path IS NOT NULL)
     OR EXISTS (SELECT 1 FROM workspace_sync_sessions
                WHERE cardinality(empty_directories) > 0 OR cardinality(upload_empty_directories) > 0 OR
                      cardinality(download_empty_directories) > 0 OR cardinality(deleted_directories) > 0 OR
                      cardinality(upload_deleted_directories) > 0 OR cardinality(download_deleted_directories) > 0)
     OR EXISTS (SELECT 1 FROM workspace_sync_session_download_actions
                WHERE action IN ('directory_upsert', 'directory_delete'))
     OR EXISTS (SELECT 1 FROM workspace_changes
                WHERE action IN ('directory_upsert', 'directory_delete')) THEN
    RAISE EXCEPTION 'cannot roll back Workspace empty-directory data after it has been written';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE workspace_sync_session_download_actions
  DROP CONSTRAINT workspace_sync_session_download_actions_action_check,
  DROP CONSTRAINT workspace_sync_session_download_actions_check;
ALTER TABLE workspace_sync_session_download_actions
  ADD CONSTRAINT workspace_sync_session_download_actions_action_check CHECK (action IN ('upsert', 'delete')),
  ADD CONSTRAINT workspace_sync_session_download_actions_check
    CHECK ((action = 'upsert' AND sha256 ~ '^[a-f0-9]{64}$') OR
           (action = 'delete' AND sha256 IS NULL AND size_bytes = 0));
ALTER TABLE workspace_changes DROP CONSTRAINT workspace_changes_action_check;
ALTER TABLE workspace_changes DROP CONSTRAINT workspace_changes_check;
ALTER TABLE workspace_changes
  ADD CONSTRAINT workspace_changes_action_check CHECK (action IN ('upsert', 'delete')),
  ADD CONSTRAINT workspace_changes_check
    CHECK ((action = 'upsert' AND file IS NOT NULL) OR (action = 'delete' AND file IS NULL));

ALTER TABLE workspace_sync_sessions
  DROP COLUMN download_deleted_directories,
  DROP COLUMN upload_deleted_directories,
  DROP COLUMN deleted_directories,
  DROP COLUMN download_empty_directories,
  DROP COLUMN upload_empty_directories,
  DROP COLUMN empty_directories;
ALTER TABLE workspace_sync_receive_reservations
  DROP CONSTRAINT workspace_sync_receive_reservations_operation_check,
  DROP CONSTRAINT workspace_sync_receive_reservations_check,
  DROP CONSTRAINT workspace_sync_receive_reservations_check1,
  DROP CONSTRAINT workspace_sync_receive_reservations_check2;
ALTER TABLE workspace_sync_receive_reservations
  DROP COLUMN base_empty_directory_path,
  ADD CONSTRAINT workspace_sync_receive_reservations_operation_check CHECK (operation IN ('upsert', 'delete')),
  ADD CONSTRAINT workspace_sync_receive_reservations_check CHECK ((base_file_size IS NULL) = (base_file_sha256 IS NULL)),
  ADD CONSTRAINT workspace_sync_receive_reservations_check1 CHECK ((operation = 'delete' AND target_size = 0) OR operation = 'upsert'),
  ADD CONSTRAINT workspace_sync_receive_reservations_check2
    CHECK ((state IN ('reserved', 'aborting', 'aborted') AND result_revision IS NULL AND result_sha256 IS NULL AND result_mode IS NULL) OR
           (state = 'settled' AND result_revision IS NOT NULL AND
            ((operation = 'delete' AND result_sha256 IS NULL AND result_mode IS NULL) OR
             (operation = 'upsert' AND result_sha256 IS NOT NULL AND result_mode IS NOT NULL))));
DROP TABLE workspace_revision_empty_directories;
ALTER TABLE workspace_revisions DROP COLUMN directory_count;
