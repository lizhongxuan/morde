-- +goose Up
ALTER TABLE task_agent_sessions
  ADD COLUMN is_background boolean NOT NULL DEFAULT false,
  ADD COLUMN cancel_requested_at timestamptz,
  ADD CONSTRAINT task_agent_sessions_cancel_background
    CHECK (cancel_requested_at IS NULL OR is_background);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM task_agent_sessions WHERE is_background
  ) THEN
    RAISE EXCEPTION 'cannot roll back while background Agent Sessions exist';
  END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE task_agent_sessions
  DROP CONSTRAINT task_agent_sessions_cancel_background,
  DROP COLUMN cancel_requested_at,
  DROP COLUMN is_background;
