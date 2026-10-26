-- +goose Up
ALTER TABLE hosts
  ADD COLUMN runtime_credential_enc bytea,
  ADD COLUMN runtime_instance_id text;

-- +goose Down
ALTER TABLE hosts
  DROP COLUMN runtime_instance_id,
  DROP COLUMN runtime_credential_enc;
