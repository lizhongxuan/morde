-- +goose Up
-- A login stream belongs to one client instance and one installation. The
-- generation is non-secret ordering metadata; no password or token is stored.
CREATE TABLE client_login_attempts (
  owner_id text NOT NULL REFERENCES users(id),
  client_instance_id uuid NOT NULL,
  generation bigint NOT NULL CHECK (generation > 0),
  installation_id text NOT NULL UNIQUE REFERENCES client_installations(id),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (owner_id, client_instance_id)
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM client_login_attempts) THEN
    RAISE EXCEPTION 'cannot roll back ordered client login while a login stream is bound to an installation';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE client_login_attempts;
