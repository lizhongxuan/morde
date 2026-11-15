-- +goose Up
ALTER TABLE tasks
  ADD COLUMN queue_priority text NOT NULL DEFAULT 'normal'
    CHECK (queue_priority IN ('pinned', 'normal', 'background'));

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM tasks WHERE queue_priority <> 'normal') THEN
    RAISE EXCEPTION 'cannot roll back task queue priority while non-default priorities exist';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE tasks DROP COLUMN queue_priority;
