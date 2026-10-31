-- +goose Up
CREATE TABLE workspace_sync_receive_reservations (
  id text PRIMARY KEY CHECK (id ~ '^r[a-z0-9]{7}$'),
  owner_id text NOT NULL,
  workspace_id text NOT NULL,
  operation_id text NOT NULL CHECK (operation_id ~ '^[a-f0-9]{64}$'),
  request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
  file_path text NOT NULL CHECK (file_path <> '' AND octet_length(file_path) <= 4096),
  folded_path text NOT NULL,
  operation text NOT NULL CHECK (operation IN ('upsert', 'delete')),
  target_size bigint NOT NULL CHECK (target_size BETWEEN 0 AND 314572800),
  base_revision bigint NOT NULL CHECK (base_revision >= 0),
  base_file_size bigint CHECK (base_file_size >= 0),
  base_file_sha256 text CHECK (base_file_sha256 ~ '^[a-f0-9]{64}$'),
  state text NOT NULL CHECK (state IN ('reserved', 'aborting', 'aborted', 'settled')),
  result_revision bigint CHECK (result_revision >= 0),
  result_sha256 text CHECK (result_sha256 ~ '^[a-f0-9]{64}$'),
  result_mode integer CHECK (result_mode BETWEEN 0 AND 511),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (owner_id, workspace_id, operation_id),
  UNIQUE (id, workspace_id),
  FOREIGN KEY (workspace_id, owner_id) REFERENCES workspaces(id, owner_id),
  FOREIGN KEY (workspace_id, base_revision) REFERENCES workspace_revisions(workspace_id, revision),
  FOREIGN KEY (workspace_id, result_revision) REFERENCES workspace_revisions(workspace_id, revision),
  CHECK ((base_file_size IS NULL) = (base_file_sha256 IS NULL)),
  CHECK ((operation = 'delete' AND target_size = 0) OR operation = 'upsert'),
  CHECK ((state IN ('reserved', 'aborting', 'aborted') AND result_revision IS NULL AND result_sha256 IS NULL AND result_mode IS NULL) OR
         (state = 'settled' AND result_revision IS NOT NULL AND
          ((operation = 'delete' AND result_sha256 IS NULL AND result_mode IS NULL) OR
           (operation = 'upsert' AND result_sha256 IS NOT NULL AND result_mode IS NOT NULL))))
);

CREATE UNIQUE INDEX workspace_sync_receive_one_active
  ON workspace_sync_receive_reservations(workspace_id)
  WHERE state IN ('reserved', 'aborting');

CREATE INDEX workspace_sync_receive_active_owner
  ON workspace_sync_receive_reservations(owner_id)
  WHERE state IN ('reserved', 'aborting');

CREATE TABLE workspace_sync_receive_blocks (
  reservation_id text NOT NULL,
  workspace_id text NOT NULL,
  sha256 text NOT NULL CHECK (sha256 ~ '^[a-f0-9]{64}$'),
  size_bytes bigint NOT NULL CHECK (size_bytes BETWEEN 1 AND 4194304),
  reserved_bytes bigint NOT NULL CHECK (reserved_bytes > 0),
  PRIMARY KEY (reservation_id, sha256),
  FOREIGN KEY (reservation_id, workspace_id)
    REFERENCES workspace_sync_receive_reservations(id, workspace_id) ON DELETE CASCADE,
  FOREIGN KEY (workspace_id, sha256)
    REFERENCES workspace_block_usage(workspace_id, sha256) ON DELETE CASCADE
);

CREATE INDEX workspace_sync_receive_blocks_digest
  ON workspace_sync_receive_blocks(workspace_id, sha256);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM workspace_sync_receive_reservations WHERE state IN ('reserved', 'aborting')) THEN
    RAISE EXCEPTION 'cannot roll back active Workspace receive reservations';
  END IF;
END $$;
-- +goose StatementEnd

DROP TABLE workspace_sync_receive_blocks;
DROP TABLE workspace_sync_receive_reservations;
