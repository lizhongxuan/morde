-- +goose Up
ALTER TABLE task_artifacts
  ADD COLUMN workspace_id text,
  ADD COLUMN workspace_revision bigint,
  ADD COLUMN size_bytes bigint,
  ADD COLUMN sha256 text,
  ADD CONSTRAINT task_artifacts_workspace_revision_fkey
    FOREIGN KEY (workspace_id, workspace_revision)
    REFERENCES workspace_revisions(workspace_id, revision) ON DELETE RESTRICT,
  ADD CONSTRAINT task_artifacts_workspace_proof_check CHECK (
    (workspace_id IS NULL AND workspace_revision IS NULL AND size_bytes IS NULL AND sha256 IS NULL)
    OR
    (workspace_id IS NOT NULL AND workspace_revision IS NOT NULL AND workspace_revision >= 0
      AND size_bytes IS NOT NULL AND size_bytes >= 0
      AND sha256 IS NOT NULL AND sha256 ~ '^[a-f0-9]{64}$')
  );

-- +goose Down
ALTER TABLE task_artifacts
  DROP CONSTRAINT task_artifacts_workspace_proof_check,
  DROP CONSTRAINT task_artifacts_workspace_revision_fkey,
  DROP COLUMN sha256,
  DROP COLUMN size_bytes,
  DROP COLUMN workspace_revision,
  DROP COLUMN workspace_id;
