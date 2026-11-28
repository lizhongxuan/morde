-- +goose Up
ALTER TABLE schedules
  ADD COLUMN file_delivery_intent text NOT NULL DEFAULT 'none'
  CHECK (file_delivery_intent IN ('none', 'required', 'unclear'));

-- +goose Down
ALTER TABLE schedules DROP COLUMN file_delivery_intent;
