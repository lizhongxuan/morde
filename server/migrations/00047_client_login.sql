-- +goose Up
-- Old pairing codes are no longer accepted; retain historical audit rows.
DELETE FROM client_authorizations;
UPDATE client_installations SET revoked_at=COALESCE(revoked_at,now()) WHERE revoked_at IS NULL;
UPDATE tokens SET revoked_at=COALESCE(revoked_at,now())
  WHERE client_installation_id IS NOT NULL AND revoked_at IS NULL;

CREATE TABLE client_login_requests (
  owner_id text NOT NULL REFERENCES users(id),
  key_hash bytea NOT NULL CHECK (octet_length(key_hash)=32),
  request_hash bytea NOT NULL CHECK (octet_length(request_hash)=32),
  installation_id text NOT NULL REFERENCES client_installations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (owner_id,key_hash)
);

ALTER TABLE tokens DROP CONSTRAINT tokens_client_installation_kind;
ALTER TABLE tokens ADD CONSTRAINT tokens_client_installation_kind CHECK (
  client_installation_id IS NULL OR kind IN ('session','client','device','ticket')
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM client_login_requests)
     OR EXISTS (SELECT 1 FROM tokens WHERE kind='session' AND client_installation_id IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot roll back while client login records or installation-bound Web sessions exist';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE tokens DROP CONSTRAINT tokens_client_installation_kind;
ALTER TABLE tokens ADD CONSTRAINT tokens_client_installation_kind CHECK (
  client_installation_id IS NULL OR kind IN ('client','device','ticket')
);
DROP TABLE client_login_requests;
