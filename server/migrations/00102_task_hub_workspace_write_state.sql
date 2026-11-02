-- +goose Up
ALTER TABLE task_hub_operations
  ADD COLUMN workspace_id text,
  ADD COLUMN workspace_path text,
  ADD COLUMN workspace_size_bytes bigint,
  ADD COLUMN workspace_sha256 text,
  ADD COLUMN source_checkpoint_revision bigint,
  ADD COLUMN local_receipt_hash bytea,
  ADD COLUMN sync_stage text NOT NULL DEFAULT '',
  ADD COLUMN sync_session_id text,
  ADD COLUMN sync_revision bigint;

ALTER TABLE task_hub_operations
  ADD CONSTRAINT task_hub_operations_workspace_owner_fkey
    FOREIGN KEY (workspace_id, owner_id) REFERENCES workspaces(id, owner_id),
  ADD CONSTRAINT task_hub_operations_workspace_checkpoint_fkey
    FOREIGN KEY (workspace_id, source_checkpoint_revision) REFERENCES workspace_revisions(workspace_id, revision),
  ADD CONSTRAINT task_hub_operations_workspace_sync_session_fkey
    FOREIGN KEY (workspace_id, sync_session_id) REFERENCES workspace_sync_sessions(workspace_id, id),
  ADD CONSTRAINT task_hub_operations_workspace_write_state_check CHECK (
    (
      action = 'file.put'
      AND workspace_id IS NOT NULL
      AND workspace_id ~ '^w[a-z0-9]{7}$'
      AND workspace_path IS NOT NULL
      AND workspace_path <> ''
      AND octet_length(workspace_path) BETWEEN 1 AND 1024
      AND workspace_path !~ '(^/|~|//|/$|^[A-Za-z]:|(^|/)([.]|[.][.])(/|$))'
      AND position(chr(92) IN workspace_path) = 0
      AND workspace_size_bytes IS NOT NULL
      AND workspace_size_bytes BETWEEN 0 AND 314572800
      AND workspace_sha256 IS NOT NULL
      AND workspace_sha256 ~ '^[a-f0-9]{64}$'
      AND source_checkpoint_revision IS NOT NULL
      AND source_checkpoint_revision >= 0
      AND sync_stage IN ('pending', 'local_applied', 'sync_pending', 'cloud_synced')
      AND (
        (sync_stage = 'pending' AND local_receipt_hash IS NULL AND sync_session_id IS NULL AND sync_revision IS NULL
          AND status IN ('prepared', 'dispatched', 'failed', 'unknown'))
        OR (sync_stage = 'local_applied' AND local_receipt_hash IS NOT NULL AND octet_length(local_receipt_hash) = 32 AND sync_session_id IS NULL AND sync_revision IS NULL
          AND status IN ('dispatched', 'unknown'))
        OR (sync_stage = 'sync_pending' AND local_receipt_hash IS NOT NULL AND octet_length(local_receipt_hash) = 32 AND sync_session_id IS NOT NULL AND sync_revision IS NULL
          AND status IN ('dispatched', 'unknown'))
        OR (sync_stage = 'cloud_synced' AND local_receipt_hash IS NOT NULL AND octet_length(local_receipt_hash) = 32 AND sync_session_id IS NOT NULL AND sync_revision IS NOT NULL AND sync_revision >= 0
          AND status = 'succeeded')
      )
    )
    OR (
      action <> 'file.put'
      AND workspace_id IS NULL AND workspace_path IS NULL AND workspace_size_bytes IS NULL
      AND workspace_sha256 IS NULL AND source_checkpoint_revision IS NULL
      AND local_receipt_hash IS NULL AND sync_stage = '' AND sync_session_id IS NULL AND sync_revision IS NULL
    )
  ),
  ADD CONSTRAINT task_hub_operations_workspace_receipt_hash_check
    CHECK (local_receipt_hash IS NULL OR octet_length(local_receipt_hash) = 32),
  ADD CONSTRAINT task_hub_operations_workspace_revision_check
    CHECK (sync_revision IS NULL OR sync_revision >= 0),
  ADD CONSTRAINT task_hub_operations_file_put_terminal_check
    CHECK ((action = 'file.put' AND (status = 'succeeded') = (sync_stage = 'cloud_synced')) OR action <> 'file.put');

CREATE INDEX task_hub_operations_active_workspace_file_put_idx
  ON task_hub_operations(workspace_id, task_id, operation_key)
  WHERE action = 'file.put' AND status IN ('prepared', 'dispatched', 'unknown');

ALTER TABLE workspace_sync_sessions
  ADD COLUMN task_id text,
  ADD COLUMN task_hub_operation_key text,
  ADD CONSTRAINT workspace_sync_sessions_task_hub_operation_pair_check
    CHECK ((task_id IS NULL) = (task_hub_operation_key IS NULL)),
  ADD CONSTRAINT workspace_sync_sessions_task_hub_operation_fkey
    FOREIGN KEY (owner_id, task_id, task_hub_operation_key)
    REFERENCES task_hub_operations(owner_id, task_id, operation_key);

CREATE INDEX workspace_sync_sessions_task_hub_operation_idx
  ON workspace_sync_sessions(owner_id, task_id, task_hub_operation_key)
  WHERE task_id IS NOT NULL;

-- +goose Down
DROP INDEX workspace_sync_sessions_task_hub_operation_idx;
DROP INDEX task_hub_operations_active_workspace_file_put_idx;

ALTER TABLE workspace_sync_sessions
  DROP CONSTRAINT workspace_sync_sessions_task_hub_operation_fkey,
  DROP CONSTRAINT workspace_sync_sessions_task_hub_operation_pair_check,
  DROP COLUMN task_hub_operation_key,
  DROP COLUMN task_id;

ALTER TABLE task_hub_operations
  DROP CONSTRAINT task_hub_operations_workspace_owner_fkey,
  DROP CONSTRAINT task_hub_operations_workspace_checkpoint_fkey,
  DROP CONSTRAINT task_hub_operations_workspace_sync_session_fkey,
  DROP CONSTRAINT task_hub_operations_workspace_write_state_check,
  DROP CONSTRAINT task_hub_operations_workspace_receipt_hash_check,
  DROP CONSTRAINT task_hub_operations_workspace_revision_check,
  DROP CONSTRAINT task_hub_operations_file_put_terminal_check,
  DROP COLUMN sync_revision,
  DROP COLUMN sync_session_id,
  DROP COLUMN sync_stage,
  DROP COLUMN local_receipt_hash,
  DROP COLUMN source_checkpoint_revision,
  DROP COLUMN workspace_sha256,
  DROP COLUMN workspace_size_bytes,
  DROP COLUMN workspace_path,
  DROP COLUMN workspace_id;
