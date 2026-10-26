-- +goose Up
-- Existing active download sessions cannot be upgraded safely: the old schema
-- did not persist planned destination deletions or file-application receipts.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM workspace_sync_sessions
    WHERE direction IN ('download', 'bidirectional')
      AND state IN ('planned', 'transferring', 'verifying', 'applying')
  ) THEN
    RAISE EXCEPTION 'cancel active download sync sessions before applying workspace download action confirmations';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE workspace_sync_sessions
  ADD COLUMN download_deleted_paths text[] NOT NULL DEFAULT '{}';

CREATE TABLE workspace_sync_session_download_actions (
  workspace_id text NOT NULL,
  session_id text NOT NULL,
  file_path text NOT NULL CHECK (file_path <> '' AND octet_length(file_path) <= 4096),
  action text NOT NULL CHECK (action IN ('upsert', 'delete')),
  sha256 text,
  size_bytes bigint NOT NULL CHECK (size_bytes >= 0),
  applied boolean NOT NULL DEFAULT false,
  PRIMARY KEY (workspace_id, session_id, file_path),
  FOREIGN KEY (workspace_id, session_id) REFERENCES workspace_sync_sessions(workspace_id, id) ON DELETE CASCADE,
  CHECK (
    (action = 'upsert' AND sha256 ~ '^[a-f0-9]{64}$') OR
    (action = 'delete' AND sha256 IS NULL AND size_bytes = 0)
  )
);
CREATE INDEX workspace_sync_download_actions_pending
  ON workspace_sync_session_download_actions(workspace_id, session_id, applied)
  WHERE applied = false;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM workspace_sync_session_download_actions) THEN
    RAISE EXCEPTION 'cannot roll back workspace download confirmations after actions have been planned';
  END IF;
END $$;
-- +goose StatementEnd

DROP TABLE workspace_sync_session_download_actions;
ALTER TABLE workspace_sync_sessions DROP COLUMN download_deleted_paths;
