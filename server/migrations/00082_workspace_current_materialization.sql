-- +goose Up
CREATE TABLE workspace_current_materializations (
  workspace_id text PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
  desired_revision bigint NOT NULL CHECK (desired_revision >= 0),
  applied_revision bigint NOT NULL DEFAULT -1 CHECK (applied_revision >= -1),
  updated_at timestamptz NOT NULL DEFAULT now(),
  CHECK (applied_revision <= desired_revision),
  FOREIGN KEY (workspace_id, desired_revision) REFERENCES workspace_revisions(workspace_id, revision)
);

INSERT INTO workspace_current_materializations (workspace_id, desired_revision, applied_revision)
SELECT id, revision, -1 FROM workspaces;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM workspace_current_materializations
    WHERE applied_revision >= 0 OR desired_revision > 0
  ) THEN
    RAISE EXCEPTION 'cannot remove Workspace current materialization state after files were applied';
  END IF;
END $$;
-- +goose StatementEnd

DROP TABLE workspace_current_materializations;
