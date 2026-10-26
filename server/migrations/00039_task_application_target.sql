-- +goose Up
ALTER TABLE tasks ADD COLUMN application_id text NOT NULL DEFAULT ''
  CHECK (application_id IN ('', 'wechat'));

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM tasks WHERE application_id <> '') THEN
    RAISE EXCEPTION 'cannot drop application target from existing tasks';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE tasks DROP COLUMN application_id;
