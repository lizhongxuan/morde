-- +goose Up
ALTER TABLE workspace_legacy_volumes
  ADD COLUMN import_error text;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM workspace_legacy_volumes WHERE import_error IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot remove legacy workspace cleanup records before they are resolved';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE workspace_legacy_volumes
  DROP COLUMN import_error;
