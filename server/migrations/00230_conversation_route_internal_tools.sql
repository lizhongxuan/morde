-- +goose Up
CREATE TABLE conversation_route_internal_tool_calls (
  task_id text PRIMARY KEY,
  owner_id text NOT NULL,
  message_id text NOT NULL REFERENCES conversation_messages(id) ON DELETE RESTRICT,
  call_id text NOT NULL CHECK (char_length(call_id) BETWEEN 1 AND 256),
  tool_name text NOT NULL CHECK (tool_name IN ('morde_web_search', 'morde_web_read')),
  arguments jsonb NOT NULL CHECK (jsonb_typeof(arguments) = 'object'),
  reply_message_id text NOT NULL UNIQUE,
  reply_published_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  UNIQUE (owner_id, message_id),
  FOREIGN KEY (task_id, owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE
);

CREATE INDEX conversation_route_internal_tool_pending_reply
  ON conversation_route_internal_tool_calls(task_id)
  WHERE reply_published_at IS NULL;

-- +goose Down
DROP TABLE conversation_route_internal_tool_calls;
