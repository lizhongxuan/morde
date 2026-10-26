-- +goose Up
CREATE TABLE users (
  id               text PRIMARY KEY,
  name             text NOT NULL UNIQUE,
  password_hash    text NOT NULL,
  role             text NOT NULL CHECK (role IN ('user', 'admin')),
  quota_hosts      integer NOT NULL DEFAULT 3,
  quota_cpu_milli   integer NOT NULL DEFAULT 4000,
  quota_mem_mib     integer NOT NULL DEFAULT 8192,
  disabled          boolean NOT NULL DEFAULT false,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE workspaces (
  id               text PRIMARY KEY,
  owner_id         text NOT NULL REFERENCES users(id),
  name             text NOT NULL DEFAULT '',
  volume_name      text NOT NULL UNIQUE,
  size_mib         integer NOT NULL,
  state            text NOT NULL CHECK (state IN ('attached', 'detached', 'deleting', 'deleted')),
  attached_host_id text,
  last_error       text NOT NULL DEFAULT '',
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  deleted_at       timestamptz
);
CREATE UNIQUE INDEX workspaces_one_per_host ON workspaces(attached_host_id) WHERE attached_host_id IS NOT NULL;

CREATE TABLE hosts (
  id                  text PRIMARY KEY,
  owner_id            text NOT NULL REFERENCES users(id),
  name                text NOT NULL DEFAULT '',
  template_id         text NOT NULL,
  runtime_profile     text NOT NULL,
  workspace_id        text REFERENCES workspaces(id),
  sandbox_id          text,
  create_attempted_at timestamptz,
  state               text NOT NULL CHECK (state IN ('creating', 'running', 'failed', 'deleting', 'deleted')),
  state_reason        text NOT NULL DEFAULT '',
  keep_workspace      boolean,
  cpu_milli           integer NOT NULL,
  mem_mib             integer NOT NULL,
  cpu_limit_milli     integer NOT NULL,
  mem_limit_mib       integer NOT NULL,
  aio_key_enc         bytea NOT NULL,
  expires_at          timestamptz,
  last_active_at      timestamptz,
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  deleted_at          timestamptz
);
CREATE INDEX hosts_owner_state ON hosts(owner_id, state);
CREATE INDEX hosts_state ON hosts(state);

CREATE TABLE tokens (
  id           text PRIMARY KEY,
  owner_id     text NOT NULL REFERENCES users(id),
  kind         text NOT NULL CHECK (kind IN ('session', 'api_key', 'ticket')),
  host_id      text REFERENCES hosts(id),
  name         text NOT NULL DEFAULT '',
  secret_hash  bytea NOT NULL,
  expires_at   timestamptz NOT NULL,
  used_at      timestamptz,
  last_used_at timestamptz,
  revoked_at   timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX tokens_owner_kind ON tokens(owner_id, kind);

CREATE TABLE audit_logs (
  id         bigserial PRIMARY KEY,
  at         timestamptz NOT NULL DEFAULT now(),
  actor_id   text NOT NULL DEFAULT '',
  actor_kind text NOT NULL CHECK (actor_kind IN ('session', 'api_key', 'host_cookie', 'system')),
  action     text NOT NULL,
  target     text NOT NULL DEFAULT '',
  detail     jsonb NOT NULL DEFAULT '{}',
  ip         text NOT NULL DEFAULT ''
);
CREATE INDEX audit_logs_at ON audit_logs(at DESC);

-- +goose Down
DROP TABLE audit_logs;
DROP TABLE tokens;
DROP TABLE hosts;
DROP TABLE workspaces;
DROP TABLE users;
