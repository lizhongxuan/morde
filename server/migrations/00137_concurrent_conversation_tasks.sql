-- +goose Up
DROP INDEX tasks_one_active_per_conversation;
CREATE INDEX tasks_active_per_conversation ON tasks(conversation_id, created_at DESC, id DESC)
  WHERE status IN ('queued', 'running', 'waiting_user', 'pausing', 'paused', 'cancelling');

-- +goose Down
DROP INDEX tasks_active_per_conversation;
-- Creating the old constraint fails safely if a conversation still has multiple active tasks.
CREATE UNIQUE INDEX tasks_one_active_per_conversation ON tasks(conversation_id)
  WHERE status IN ('queued', 'running', 'waiting_user', 'pausing', 'paused', 'cancelling');
