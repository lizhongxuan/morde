-- +goose Up
CREATE TABLE client_installations (
  id text PRIMARY KEY CHECK (id ~ '^i[a-z0-9]{7}$'),
  owner_id text NOT NULL REFERENCES users(id),
  display_name text NOT NULL CHECK (octet_length(display_name) BETWEEN 1 AND 100),
  platform text NOT NULL CHECK (platform IN ('desktop', 'android', 'ios')),
  workspace_ids text[] NOT NULL DEFAULT '{}'::text[] CHECK (cardinality(workspace_ids) <= 500),
  expires_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  last_used_at timestamptz,
  revoked_at timestamptz
);
CREATE INDEX client_installations_owner ON client_installations(owner_id, created_at DESC);

CREATE TABLE client_authorizations (
  id text PRIMARY KEY CHECK (id ~ '^t[a-z0-9]{7}$'),
  display_name text NOT NULL CHECK (octet_length(display_name) BETWEEN 1 AND 100),
  platform text NOT NULL CHECK (platform IN ('desktop', 'android', 'ios')),
  wants_workspace_sync boolean NOT NULL,
  renewal_installation_id text REFERENCES client_installations(id),
  secret_hash bytea NOT NULL CHECK (octet_length(secret_hash) = 32),
  user_code_hash bytea NOT NULL UNIQUE CHECK (octet_length(user_code_hash) = 32),
  state text NOT NULL CHECK (state IN ('pending', 'approved', 'denied', 'claimed')),
  installation_id text REFERENCES client_installations(id),
  owner_id text REFERENCES users(id),
  workspace_ids text[] NOT NULL DEFAULT '{}'::text[] CHECK (cardinality(workspace_ids) <= 500),
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  claimed_at timestamptz,
  CHECK (wants_workspace_sync OR cardinality(workspace_ids) = 0)
);
CREATE INDEX client_authorizations_pending_expiry ON client_authorizations(expires_at) WHERE state = 'pending';

ALTER TABLE tokens ADD COLUMN client_installation_id text REFERENCES client_installations(id);
CREATE INDEX tokens_client_installation ON tokens(client_installation_id) WHERE client_installation_id IS NOT NULL;
ALTER TABLE tokens DROP CONSTRAINT tokens_kind_check;
ALTER TABLE tokens ADD CONSTRAINT tokens_kind_check
  CHECK (kind IN ('session', 'api_key', 'ticket', 'device', 'client'));
ALTER TABLE tokens DROP CONSTRAINT tokens_grants_api_key_only;
ALTER TABLE tokens ADD CONSTRAINT tokens_grants_api_key_only CHECK (
  (kind = 'api_key' AND client_installation_id IS NULL AND cardinality(workspace_ids) = 0 AND NOT ('workspace:sync' = ANY(scopes)))
  OR (kind = 'device' AND host_id IS NULL AND cardinality(host_ids) = 0
      AND scopes = ARRAY['workspace:sync']::text[] AND cardinality(workspace_ids) <= 500)
  OR (kind = 'client' AND client_installation_id IS NOT NULL AND host_id IS NULL
      AND cardinality(host_ids) = 0 AND cardinality(workspace_ids) = 0 AND cardinality(scopes) = 0)
  OR (kind NOT IN ('api_key', 'device', 'client') AND cardinality(host_ids) = 0
      AND cardinality(workspace_ids) = 0 AND cardinality(scopes) = 0)
);
ALTER TABLE tokens ADD CONSTRAINT tokens_client_installation_kind CHECK (
  client_installation_id IS NULL OR kind IN ('client', 'device', 'ticket')
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM client_installations) THEN
    RAISE EXCEPTION 'cannot roll back client installations while installations exist';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE tokens DROP CONSTRAINT tokens_client_installation_kind;
ALTER TABLE tokens DROP CONSTRAINT tokens_grants_api_key_only;
ALTER TABLE tokens ADD CONSTRAINT tokens_grants_api_key_only CHECK (
  (kind = 'api_key' AND cardinality(workspace_ids) = 0 AND NOT ('workspace:sync' = ANY(scopes)))
  OR (kind = 'device' AND host_id IS NULL AND cardinality(host_ids) = 0
      AND scopes = ARRAY['workspace:sync']::text[] AND cardinality(workspace_ids) <= 500)
  OR (kind NOT IN ('api_key', 'device') AND cardinality(host_ids) = 0
      AND cardinality(workspace_ids) = 0 AND cardinality(scopes) = 0)
);
ALTER TABLE tokens DROP CONSTRAINT tokens_kind_check;
ALTER TABLE tokens ADD CONSTRAINT tokens_kind_check CHECK (kind IN ('session', 'api_key', 'ticket', 'device'));
ALTER TABLE tokens DROP COLUMN client_installation_id;
DROP TABLE client_authorizations;
DROP TABLE client_installations;
