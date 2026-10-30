-- +goose Up
CREATE TABLE workspace_sync_commit_receipts (
  owner_id     text NOT NULL REFERENCES users(id),
  operation    text NOT NULL CHECK (operation = 'workspace.sync.commit'),
  key          text NOT NULL,
  request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
  workspace_id text NOT NULL,
  session_id   text NOT NULL,
  state        text NOT NULL CHECK (state IN ('pending', 'succeeded', 'quota_rejected')),
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (owner_id, operation, key)
);

-- +goose Down
DROP TABLE workspace_sync_commit_receipts;
