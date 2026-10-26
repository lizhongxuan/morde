-- +goose Up
ALTER TABLE mcp_runtime_sessions ADD COLUMN actor_user_id text;
UPDATE mcp_runtime_sessions SET actor_user_id=owner_id;
ALTER TABLE mcp_runtime_sessions ALTER COLUMN actor_user_id SET NOT NULL;

ALTER TABLE mcp_runtime_sessions DROP CONSTRAINT mcp_runtime_sessions_owner_id_api_key_id_fkey;
ALTER TABLE mcp_runtime_sessions
  ADD CONSTRAINT mcp_runtime_sessions_actor_api_key_fkey
  FOREIGN KEY (actor_user_id, api_key_id) REFERENCES tokens(owner_id, id) ON DELETE RESTRICT;
ALTER TABLE mcp_runtime_sessions
  ADD CONSTRAINT mcp_runtime_sessions_owner_actor_key_session_host_key
  UNIQUE (owner_id, actor_user_id, api_key_id, session_id, host_id);

ALTER TABLE mcp_runtime_operations ADD COLUMN actor_user_id text;
UPDATE mcp_runtime_operations o
SET actor_user_id=s.actor_user_id
FROM mcp_runtime_sessions s
WHERE s.owner_id=o.owner_id AND s.api_key_id=o.api_key_id AND s.session_id=o.session_id AND s.host_id=o.host_id;
ALTER TABLE mcp_runtime_operations ALTER COLUMN actor_user_id SET NOT NULL;
ALTER TABLE mcp_runtime_operations
  ADD CONSTRAINT mcp_runtime_operations_actor_session_fkey
  FOREIGN KEY (owner_id, actor_user_id, api_key_id, session_id, host_id)
  REFERENCES mcp_runtime_sessions(owner_id, actor_user_id, api_key_id, session_id, host_id) ON DELETE CASCADE;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM mcp_runtime_sessions WHERE actor_user_id <> owner_id) THEN
    RAISE EXCEPTION 'cannot roll back MCP runtime actor identity while member operations exist';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE mcp_runtime_operations DROP CONSTRAINT mcp_runtime_operations_actor_session_fkey;
ALTER TABLE mcp_runtime_operations DROP COLUMN actor_user_id;
ALTER TABLE mcp_runtime_sessions DROP CONSTRAINT mcp_runtime_sessions_owner_actor_key_session_host_key;
ALTER TABLE mcp_runtime_sessions DROP CONSTRAINT mcp_runtime_sessions_actor_api_key_fkey;
ALTER TABLE mcp_runtime_sessions ADD CONSTRAINT mcp_runtime_sessions_owner_id_api_key_id_fkey
  FOREIGN KEY (owner_id, api_key_id) REFERENCES tokens(owner_id, id) ON DELETE RESTRICT;
ALTER TABLE mcp_runtime_sessions DROP COLUMN actor_user_id;
