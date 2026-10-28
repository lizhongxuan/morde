-- +goose Up
ALTER TABLE task_agent_sessions
  ADD COLUMN allowed_tools jsonb NOT NULL DEFAULT '[]'::jsonb,
  ADD CONSTRAINT task_agent_sessions_allowed_tools_array
    CHECK (jsonb_typeof(allowed_tools) = 'array');

-- +goose Down
ALTER TABLE task_agent_sessions
  DROP CONSTRAINT task_agent_sessions_allowed_tools_array,
  DROP COLUMN allowed_tools;
