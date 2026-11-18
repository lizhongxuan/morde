-- +goose Up
CREATE TABLE user_agent_workspaces (
  owner_id text PRIMARY KEY REFERENCES users(id) ON DELETE RESTRICT,
  workspace_id text NOT NULL UNIQUE,
  directory_initialized_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  CONSTRAINT user_agent_workspaces_workspace_owner_fk
    FOREIGN KEY (workspace_id, owner_id) REFERENCES workspaces(id, owner_id)
    ON UPDATE RESTRICT ON DELETE RESTRICT
);

-- Retired Agent Workspaces stay recognizable while detached Host cleanup is
-- still pending, even after the owner explicitly resets the current mapping.
CREATE TABLE agent_workspace_history (
  workspace_id text PRIMARY KEY,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  CONSTRAINT agent_workspace_history_workspace_owner_fk
    FOREIGN KEY (workspace_id, owner_id) REFERENCES workspaces(id, owner_id)
    ON UPDATE RESTRICT ON DELETE RESTRICT
);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM user_agent_workspaces) THEN
    RAISE EXCEPTION 'cannot remove stable Agent Workspace mappings after initialization';
  END IF;
  IF EXISTS (SELECT 1 FROM agent_workspace_history) THEN
    RAISE EXCEPTION 'cannot remove Agent Workspace history after initialization';
  END IF;
END;
$$;
-- +goose StatementEnd

DROP TABLE agent_workspace_history;
DROP TABLE user_agent_workspaces;
