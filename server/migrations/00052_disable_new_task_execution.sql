-- +goose Up
ALTER TABLE owner_execution_controls ALTER COLUMN state SET DEFAULT 'disabled';

-- +goose Down
ALTER TABLE owner_execution_controls ALTER COLUMN state SET DEFAULT 'enabled';
