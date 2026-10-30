-- +goose Up
CREATE TABLE workspace_sync_device_identities (
  id text PRIMARY KEY CHECK (id ~ '^e[a-z0-9]{7}$'),
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  client_installation_id text NOT NULL REFERENCES client_installations(id) ON DELETE CASCADE,
  device_id text NOT NULL CHECK (device_id ~ '^[A-Z2-7]{7}(-[A-Z2-7]{7}){7}$'),
  certificate_sha256 bytea NOT NULL CHECK (octet_length(certificate_sha256) = 32),
  created_at timestamptz NOT NULL DEFAULT now(),
  revoked_at timestamptz,
  UNIQUE (id, owner_id, client_installation_id)
);
CREATE UNIQUE INDEX workspace_sync_device_identity_active_device
  ON workspace_sync_device_identities(device_id) WHERE revoked_at IS NULL;
CREATE INDEX workspace_sync_device_identity_installation
  ON workspace_sync_device_identities(client_installation_id) WHERE revoked_at IS NULL;

CREATE TABLE workspace_sync_folders (
  workspace_id text PRIMARY KEY,
  owner_id text NOT NULL,
  folder_id text NOT NULL UNIQUE CHECK (folder_id ~ '^morde-[a-f0-9]{32}$'),
  state text NOT NULL DEFAULT 'pending_node' CHECK (state IN ('pending_node', 'active', 'revoked')),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (workspace_id, owner_id),
  FOREIGN KEY (workspace_id, owner_id) REFERENCES workspaces(id, owner_id) ON DELETE CASCADE
);

CREATE TABLE workspace_sync_device_bindings (
  id text PRIMARY KEY CHECK (id ~ '^d[a-z0-9]{7}$'),
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  workspace_id text NOT NULL,
  client_installation_id text NOT NULL REFERENCES client_installations(id) ON DELETE CASCADE,
  device_identity_id text NOT NULL,
  state text NOT NULL DEFAULT 'pending_node' CHECK (state IN ('pending_node', 'active', 'revoked')),
  created_at timestamptz NOT NULL DEFAULT now(),
  revoked_at timestamptz,
  FOREIGN KEY (workspace_id, owner_id) REFERENCES workspaces(id, owner_id) ON DELETE CASCADE,
  FOREIGN KEY (workspace_id, owner_id) REFERENCES workspace_sync_folders(workspace_id, owner_id) ON DELETE CASCADE,
  FOREIGN KEY (device_identity_id, owner_id, client_installation_id)
    REFERENCES workspace_sync_device_identities(id, owner_id, client_installation_id) ON DELETE CASCADE,
  UNIQUE (id, owner_id, workspace_id)
);
CREATE UNIQUE INDEX workspace_sync_device_binding_active_workspace
  ON workspace_sync_device_bindings(workspace_id, device_identity_id) WHERE state <> 'revoked';
CREATE INDEX workspace_sync_device_binding_installation
  ON workspace_sync_device_bindings(client_installation_id, state);

CREATE TABLE workspace_sync_device_challenges (
  id text PRIMARY KEY CHECK (id ~ '^n[a-z0-9]{7}$'),
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  workspace_id text NOT NULL,
  client_installation_id text NOT NULL REFERENCES client_installations(id) ON DELETE CASCADE,
  device_id text NOT NULL CHECK (device_id ~ '^[A-Z2-7]{7}(-[A-Z2-7]{7}){7}$'),
  certificate_der bytea NOT NULL CHECK (octet_length(certificate_der) BETWEEN 1 AND 16384),
  certificate_sha256 bytea NOT NULL CHECK (octet_length(certificate_sha256) = 32),
  nonce bytea NOT NULL CHECK (octet_length(nonce) = 32),
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (workspace_id, owner_id) REFERENCES workspaces(id, owner_id) ON DELETE CASCADE
);
CREATE INDEX workspace_sync_device_challenges_expiry ON workspace_sync_device_challenges(expires_at);

CREATE TABLE workspace_sync_device_registration_requests (
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  workspace_id text NOT NULL,
  idempotency_key_hash bytea NOT NULL CHECK (octet_length(idempotency_key_hash) = 32),
  request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
  binding_id text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (owner_id, workspace_id, idempotency_key_hash),
  FOREIGN KEY (workspace_id, owner_id) REFERENCES workspaces(id, owner_id) ON DELETE CASCADE,
  FOREIGN KEY (binding_id, owner_id, workspace_id)
    REFERENCES workspace_sync_device_bindings(id, owner_id, workspace_id)
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM workspace_sync_device_bindings)
     OR EXISTS (SELECT 1 FROM workspace_sync_device_challenges)
     OR EXISTS (SELECT 1 FROM workspace_sync_device_identities) THEN
    RAISE EXCEPTION 'cannot roll back Workspace Syncthing device bindings after device registration';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE workspace_sync_device_registration_requests;
DROP TABLE workspace_sync_device_challenges;
DROP TABLE workspace_sync_device_bindings;
DROP TABLE workspace_sync_folders;
DROP TABLE workspace_sync_device_identities;
