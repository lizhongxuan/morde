-- +goose Up
-- The PC snapshot remains the deletion-ownership baseline; this separate
-- snapshot records what was actually applied on a direct-write cloud target.
CREATE TABLE workspace_sync_project_live_baselines (
  workspace_id text NOT NULL,
  session_id text NOT NULL,
  files jsonb NOT NULL CHECK (jsonb_typeof(files) = 'array'),
  directories jsonb NOT NULL CHECK (jsonb_typeof(directories) = 'array'),
  PRIMARY KEY (workspace_id, session_id),
  FOREIGN KEY (workspace_id, session_id)
    REFERENCES workspace_sync_sessions(workspace_id, id) ON DELETE RESTRICT
);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM workspace_sync_project_live_baselines) THEN
    RAISE EXCEPTION 'cannot discard applied project cloud baselines';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE workspace_sync_project_live_baselines;
