-- +goose Up
CREATE TABLE owner_execution_controls (
  owner_id   text PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  state      text NOT NULL DEFAULT 'enabled' CHECK (state IN ('enabled', 'stopping', 'disabled')),
  epoch      bigint NOT NULL DEFAULT 1 CHECK (epoch > 0),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

INSERT INTO owner_execution_controls (owner_id)
SELECT id FROM users
ON CONFLICT (owner_id) DO NOTHING;

-- +goose StatementBegin
CREATE FUNCTION create_owner_execution_control() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO owner_execution_controls (owner_id) VALUES (NEW.id)
  ON CONFLICT (owner_id) DO NOTHING;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER users_create_execution_control
AFTER INSERT ON users
FOR EACH ROW EXECUTE FUNCTION create_owner_execution_control();

ALTER TABLE tasks ADD COLUMN execution_epoch bigint NOT NULL DEFAULT 1 CHECK (execution_epoch > 0);

-- +goose Down
DROP TRIGGER users_create_execution_control ON users;
DROP FUNCTION create_owner_execution_control();
ALTER TABLE tasks DROP COLUMN execution_epoch;
DROP TABLE owner_execution_controls;
