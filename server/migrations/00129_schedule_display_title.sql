-- +goose Up
ALTER TABLE schedules ADD COLUMN title text;
UPDATE schedules SET title = objective WHERE title IS NULL;
ALTER TABLE schedules ALTER COLUMN title SET NOT NULL;
ALTER TABLE schedules ADD CONSTRAINT schedules_title_valid
  CHECK (length(btrim(title)) > 0 AND octet_length(title) <= 32768);

-- +goose Down
-- Refuse rollback if it would discard user-edited display titles.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM schedules WHERE title <> objective) THEN
    RAISE EXCEPTION 'cannot drop schedule titles with custom values';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE schedules DROP CONSTRAINT schedules_title_valid;
ALTER TABLE schedules DROP COLUMN title;
