-- +goose Up
CREATE TABLE admin_model_defaults (
  id smallint PRIMARY KEY CHECK (id = 1),
  model_connection_id text REFERENCES model_connections(id) ON DELETE RESTRICT,
  version bigint NOT NULL CHECK (version >= 0),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  CHECK (model_connection_id IS NULL OR version > 0)
);

INSERT INTO admin_model_defaults (id, model_connection_id, version)
VALUES (1, NULL, 0);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM admin_model_defaults WHERE model_connection_id IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot roll back a configured default model';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE admin_model_defaults;
