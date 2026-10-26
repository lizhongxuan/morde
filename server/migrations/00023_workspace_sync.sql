-- +goose Up
CREATE TABLE workspace_revisions (
  workspace_id text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  revision bigint NOT NULL CHECK (revision >= 0),
  file_count bigint NOT NULL CHECK (file_count >= 0),
  total_bytes bigint NOT NULL CHECK (total_bytes >= 0),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, revision)
);

-- Every existing workspace starts with an empty, immutable manifest. Historical
-- Docker volume contents are imported by a separate verified recovery flow.
INSERT INTO workspace_revisions (workspace_id, revision, file_count, total_bytes)
SELECT id, revision, 0, 0 FROM workspaces;

CREATE TABLE workspace_revision_files (
  workspace_id text NOT NULL,
  valid_from_revision bigint NOT NULL,
  valid_to_revision bigint,
  file_path text NOT NULL CHECK (file_path <> '' AND octet_length(file_path) <= 4096),
  folded_path text NOT NULL,
  size_bytes bigint NOT NULL CHECK (size_bytes >= 0),
  sha256 text NOT NULL CHECK (sha256 ~ '^[a-f0-9]{64}$'),
  mode integer NOT NULL CHECK (mode BETWEEN 0 AND 511),
  chunks jsonb NOT NULL CHECK (jsonb_typeof(chunks) = 'array'),
  PRIMARY KEY (workspace_id, file_path, valid_from_revision),
  FOREIGN KEY (workspace_id, valid_from_revision) REFERENCES workspace_revisions(workspace_id, revision) ON DELETE CASCADE,
  FOREIGN KEY (workspace_id, valid_to_revision) REFERENCES workspace_revisions(workspace_id, revision) ON DELETE CASCADE,
  CHECK (valid_to_revision IS NULL OR valid_to_revision > valid_from_revision)
);
CREATE UNIQUE INDEX workspace_revision_files_active_casefold ON workspace_revision_files(workspace_id, folded_path)
  WHERE valid_to_revision IS NULL;
CREATE INDEX workspace_revision_files_page ON workspace_revision_files(workspace_id, file_path, valid_from_revision, valid_to_revision);

CREATE TABLE workspace_changes (
  workspace_id text NOT NULL,
  revision bigint NOT NULL,
  file_path text NOT NULL,
  action text NOT NULL CHECK (action IN ('upsert', 'delete')),
  file jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, revision, file_path),
  FOREIGN KEY (workspace_id, revision) REFERENCES workspace_revisions(workspace_id, revision) ON DELETE CASCADE,
  CHECK ((action = 'upsert' AND file IS NOT NULL) OR (action = 'delete' AND file IS NULL))
);
CREATE INDEX workspace_changes_cursor ON workspace_changes(workspace_id, revision, file_path);

CREATE TABLE workspace_sync_sessions (
  id text PRIMARY KEY CHECK (id ~ '^p[a-z0-9]{7}$'),
  workspace_id text NOT NULL,
  owner_id text NOT NULL,
  replica_id text NOT NULL,
  direction text NOT NULL CHECK (direction IN ('upload', 'download', 'bidirectional')),
  policy text NOT NULL CHECK (policy IN ('fill_missing', 'compare_merge', 'live')),
  state text NOT NULL CHECK (state IN ('planned', 'transferring', 'verifying', 'applying', 'completed', 'conflict', 'cancelled', 'failed', 'expired')),
  stage text NOT NULL CHECK (stage IN ('scan', 'compare', 'transfer', 'verify', 'apply', 'complete')),
  base_revision bigint NOT NULL CHECK (base_revision >= 0),
  target_revision bigint NOT NULL CHECK (target_revision >= 0),
  pinned_revision bigint NOT NULL CHECK (pinned_revision >= 0),
  planned_bytes bigint NOT NULL CHECK (planned_bytes >= 0),
  confirmed_bytes bigint NOT NULL DEFAULT 0 CHECK (confirmed_bytes >= 0 AND confirmed_bytes <= planned_bytes),
  files jsonb NOT NULL CHECK (jsonb_typeof(files) = 'array'),
  upload_files jsonb NOT NULL CHECK (jsonb_typeof(upload_files) = 'array'),
  download_files jsonb NOT NULL CHECK (jsonb_typeof(download_files) = 'array'),
  scope text[] NOT NULL DEFAULT '{}',
  deleted_paths text[] NOT NULL DEFAULT '{}',
  upload_deleted_paths text[] NOT NULL DEFAULT '{}',
  ack_cursor text,
  conflicts jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(conflicts) = 'array'),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (workspace_id, id),
  FOREIGN KEY (workspace_id, owner_id) REFERENCES workspaces(id, owner_id) ON DELETE CASCADE,
  FOREIGN KEY (workspace_id, base_revision) REFERENCES workspace_revisions(workspace_id, revision),
  FOREIGN KEY (workspace_id, pinned_revision) REFERENCES workspace_revisions(workspace_id, revision)
);
CREATE INDEX workspace_sync_sessions_active ON workspace_sync_sessions(workspace_id, state, updated_at)
  WHERE state IN ('planned', 'transferring', 'verifying', 'applying');
CREATE INDEX workspace_sync_sessions_replica ON workspace_sync_sessions(workspace_id, replica_id, created_at DESC);

CREATE TABLE workspace_sync_session_blocks (
  workspace_id text NOT NULL,
  session_id text NOT NULL,
  sha256 text NOT NULL CHECK (sha256 ~ '^[a-f0-9]{64}$'),
  size_bytes bigint NOT NULL CHECK (size_bytes BETWEEN 1 AND 4194304),
  role text NOT NULL CHECK (role IN ('upload', 'download')),
  received boolean NOT NULL DEFAULT false,
  PRIMARY KEY (workspace_id, session_id, sha256, role),
  FOREIGN KEY (workspace_id, session_id) REFERENCES workspace_sync_sessions(workspace_id, id) ON DELETE CASCADE
);
CREATE INDEX workspace_sync_session_blocks_required ON workspace_sync_session_blocks(workspace_id, session_id, role, received);

CREATE TABLE workspace_sync_cursors (
  workspace_id text NOT NULL,
  owner_id text NOT NULL,
  replica_id text NOT NULL,
  revision bigint NOT NULL CHECK (revision >= 0),
  file_path text NOT NULL DEFAULT '',
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, replica_id),
  FOREIGN KEY (workspace_id, owner_id) REFERENCES workspaces(id, owner_id) ON DELETE CASCADE,
  FOREIGN KEY (workspace_id, revision) REFERENCES workspace_revisions(workspace_id, revision)
);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM workspace_revisions WHERE revision > 0)
     OR EXISTS (SELECT 1 FROM workspace_sync_sessions)
     OR EXISTS (SELECT 1 FROM workspace_sync_cursors) THEN
    RAISE EXCEPTION 'cannot roll back workspace sync persistence after synchronization data exists';
  END IF;
END $$;
-- +goose StatementEnd

DROP TABLE workspace_sync_cursors;
DROP TABLE workspace_sync_session_blocks;
DROP TABLE workspace_sync_sessions;
DROP TABLE workspace_changes;
DROP TABLE workspace_revision_files;
DROP TABLE workspace_revisions;
