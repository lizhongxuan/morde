-- +goose Up
CREATE TABLE task_workspace_publish_receipts (
  task_id                 text NOT NULL,
  owner_id                text NOT NULL,
  workspace_id            text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  operation_key           text NOT NULL CHECK (length(operation_key) BETWEEN 1 AND 255),
  request_hash            bytea NOT NULL CHECK (octet_length(request_hash) = 32),
  base_revision           bigint NOT NULL CHECK (base_revision >= 0),
  state                   text NOT NULL CHECK (state IN ('pending', 'succeeded', 'conflict', 'quota_rejected')),
  manifest_sha256         text CHECK (manifest_sha256 IS NULL OR manifest_sha256 ~ '^[a-f0-9]{64}$'),
  published_revision      bigint CHECK (published_revision IS NULL OR published_revision >= 0),
  created_at              timestamptz NOT NULL DEFAULT now(),
  updated_at              timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (task_id, operation_key),
  FOREIGN KEY (task_id, owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE,
  CHECK (
    (state = 'succeeded' AND manifest_sha256 IS NOT NULL AND published_revision IS NOT NULL) OR
    (state <> 'succeeded' AND manifest_sha256 IS NULL AND published_revision IS NULL)
  )
);

CREATE INDEX task_workspace_publish_receipts_pending_idx
  ON task_workspace_publish_receipts (task_id)
  WHERE state = 'pending';

-- +goose Down
DROP TABLE task_workspace_publish_receipts;
