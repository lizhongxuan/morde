-- +goose Up
ALTER TABLE tokens
  ADD COLUMN workspace_ids text[] NOT NULL DEFAULT '{}'::text[];

ALTER TABLE tokens DROP CONSTRAINT tokens_kind_check;
ALTER TABLE tokens ADD CONSTRAINT tokens_kind_check
  CHECK (kind IN ('session', 'api_key', 'ticket', 'device'));

ALTER TABLE tokens DROP CONSTRAINT tokens_scope_values;
ALTER TABLE tokens ADD CONSTRAINT tokens_scope_values CHECK (scopes <@ ARRAY[
  'host:read', 'host:create', 'host:manage', 'host:delete_data',
  'computer:observe', 'computer:operate', 'task:read', 'task:submit', 'task:control',
  'schedule:read', 'schedule:manage', 'workspace:sync'
]::text[]);

ALTER TABLE tokens DROP CONSTRAINT tokens_host_ids_limit;
ALTER TABLE tokens ADD CONSTRAINT tokens_host_ids_limit CHECK (cardinality(host_ids) <= 500);
ALTER TABLE tokens ADD CONSTRAINT tokens_workspace_ids_limit CHECK (cardinality(workspace_ids) <= 500);
ALTER TABLE tokens DROP CONSTRAINT tokens_grants_api_key_only;
ALTER TABLE tokens ADD CONSTRAINT tokens_grants_api_key_only CHECK (
  (kind = 'api_key' AND cardinality(workspace_ids) = 0 AND NOT ('workspace:sync' = ANY(scopes)))
  OR (kind = 'device' AND host_id IS NULL AND cardinality(host_ids) = 0
      AND scopes = ARRAY['workspace:sync']::text[] AND cardinality(workspace_ids) <= 500)
  OR (kind NOT IN ('api_key', 'device') AND cardinality(host_ids) = 0
      AND cardinality(workspace_ids) = 0 AND cardinality(scopes) = 0)
);

CREATE TABLE device_authorizations (
  id                 text PRIMARY KEY CHECK (id ~ '^t[a-z0-9]{7}$'),
  name               text NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 100),
  secret_hash        bytea NOT NULL CHECK (octet_length(secret_hash) = 32),
  user_code_hash     bytea NOT NULL UNIQUE CHECK (octet_length(user_code_hash) = 32),
  state              text NOT NULL CHECK (state IN ('pending', 'approved', 'denied')),
  expires_at         timestamptz NOT NULL,
  created_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX device_authorizations_pending_expiry ON device_authorizations(expires_at) WHERE state = 'pending';

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM tokens WHERE kind = 'device') THEN
    RAISE EXCEPTION 'cannot roll back device authorization while device credentials exist';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE device_authorizations;
ALTER TABLE tokens DROP CONSTRAINT tokens_grants_api_key_only;
ALTER TABLE tokens DROP CONSTRAINT tokens_workspace_ids_limit;
ALTER TABLE tokens DROP COLUMN workspace_ids;
ALTER TABLE tokens ADD CONSTRAINT tokens_grants_api_key_only CHECK (
  kind = 'api_key' OR (cardinality(host_ids) = 0 AND cardinality(scopes) = 0)
);
ALTER TABLE tokens DROP CONSTRAINT tokens_scope_values;
ALTER TABLE tokens ADD CONSTRAINT tokens_scope_values CHECK (scopes <@ ARRAY[
  'host:read', 'host:create', 'host:manage', 'host:delete_data',
  'computer:observe', 'computer:operate', 'task:read', 'task:submit', 'task:control'
]::text[]);
ALTER TABLE tokens DROP CONSTRAINT tokens_kind_check;
ALTER TABLE tokens ADD CONSTRAINT tokens_kind_check CHECK (kind IN ('session', 'api_key', 'ticket'));
