-- +goose Up
ALTER TABLE tasks
  ADD COLUMN file_delivery_intent text NOT NULL DEFAULT 'unclear',
  ADD CONSTRAINT tasks_file_delivery_intent_check
    CHECK (file_delivery_intent IN ('none', 'required', 'unclear'));

-- +goose Down
ALTER TABLE tasks
  DROP CONSTRAINT tasks_file_delivery_intent_check,
  DROP COLUMN file_delivery_intent;
