-- +goose Up
DROP INDEX tasks_one_active_per_conversation;
CREATE UNIQUE INDEX tasks_one_active_per_conversation ON tasks(conversation_id)
  WHERE status IN ('queued', 'running', 'waiting_user', 'pausing', 'paused', 'cancelling');

-- +goose Down
DROP INDEX tasks_one_active_per_conversation;
CREATE UNIQUE INDEX tasks_one_active_per_conversation ON tasks(conversation_id)
  WHERE status IN ('queued', 'running', 'waiting_user', 'pausing', 'cancelling');
