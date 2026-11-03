-- +goose Up
-- Directory permissions and timestamps are versioned independently from file
-- content. No historical values are backfilled; absent rows mean unknown.
CREATE TABLE workspace_revision_directories (
  workspace_id text NOT NULL,
  valid_from_revision bigint NOT NULL,
  valid_to_revision bigint,
  directory_path text NOT NULL CHECK (directory_path <> '' AND octet_length(directory_path) <= 4096),
  folded_path text NOT NULL,
  mode integer CHECK (mode IS NULL OR mode BETWEEN 0 AND 511),
  mod_time_ns bigint,
  PRIMARY KEY (workspace_id, directory_path, valid_from_revision),
  FOREIGN KEY (workspace_id, valid_from_revision) REFERENCES workspace_revisions(workspace_id, revision) ON DELETE CASCADE,
  FOREIGN KEY (workspace_id, valid_to_revision) REFERENCES workspace_revisions(workspace_id, revision) ON DELETE CASCADE,
  CHECK (valid_to_revision IS NULL OR valid_to_revision > valid_from_revision)
);
CREATE UNIQUE INDEX workspace_revision_directories_active_casefold
  ON workspace_revision_directories(workspace_id, folded_path)
  WHERE valid_to_revision IS NULL;
CREATE INDEX workspace_revision_directories_page
  ON workspace_revision_directories(workspace_id, folded_path, valid_from_revision, valid_to_revision);

CREATE TABLE workspace_sync_session_directories (
  workspace_id text NOT NULL,
  session_id text NOT NULL,
  directories jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(directories) = 'array'),
  upload_directories jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(upload_directories) = 'array'),
  download_directories jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(download_directories) = 'array'),
  PRIMARY KEY (workspace_id, session_id),
  FOREIGN KEY (workspace_id, session_id) REFERENCES workspace_sync_sessions(workspace_id, id) ON DELETE CASCADE
);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM workspace_revision_directories) OR
     EXISTS (SELECT 1 FROM workspace_sync_session_directories WHERE directories <> '[]'::jsonb OR upload_directories <> '[]'::jsonb OR download_directories <> '[]'::jsonb) THEN
    RAISE EXCEPTION 'cannot roll back workspace directory metadata after it has been written';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE workspace_sync_session_directories;
DROP TABLE workspace_revision_directories;
