-- +goose Up
-- A task grant may be renewed only by a later, explicit user selection.
CREATE TABLE task_mcp_selection_sources (
  task_id text NOT NULL,
  owner_id text NOT NULL,
  connector_id text NOT NULL,
  message_id text NOT NULL REFERENCES conversation_message_routes(message_id) ON DELETE RESTRICT,
  task_revision bigint NOT NULL CHECK (task_revision >= 0),
  grant_version bigint NOT NULL CHECK (grant_version > 0),
  PRIMARY KEY (task_id, connector_id),
  FOREIGN KEY (task_id, owner_id, connector_id)
    REFERENCES task_mcp_connector_grants(task_id, owner_id, connector_id) ON DELETE CASCADE
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM task_mcp_selection_sources) THEN
    RAISE EXCEPTION 'cannot remove MCP grant provenance while selected grants exist';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE task_mcp_selection_sources;
