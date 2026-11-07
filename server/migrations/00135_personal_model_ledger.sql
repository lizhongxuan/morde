-- +goose Up
ALTER TABLE model_call_ledger ADD COLUMN personal_model boolean NOT NULL DEFAULT false;

-- Preserve historical usage; known personal connections no longer consume platform quota.
UPDATE model_call_ledger AS ledger SET personal_model = true
FROM model_connections AS connection
WHERE ledger.model_id = connection.id AND connection.owner_id = ledger.owner_id
  AND connection.created_at <= ledger.created_at;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM model_call_ledger WHERE personal_model) THEN
    RAISE EXCEPTION 'cannot discard personal model ledger attribution';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE model_call_ledger DROP COLUMN personal_model;
