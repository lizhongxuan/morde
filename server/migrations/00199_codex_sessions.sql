-- +goose Up
-- Codex mirror data has no automatic TTL. The session, grant, command body and
-- normalized event history remain until an explicit session or account delete.
CREATE TABLE codex_sessions (
  id text PRIMARY KEY CHECK (id ~ '^l[a-z0-9]{7}$'),
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  installation_id text NOT NULL,
  profile_key text NOT NULL CHECK (profile_key ~ '^[A-Za-z0-9_-]{1,128}$'),
  thread_id text NOT NULL CHECK (octet_length(thread_id) BETWEEN 1 AND 512),
  title text NOT NULL DEFAULT '' CHECK (octet_length(title) <= 512),
  cwd_label text NOT NULL DEFAULT '' CHECK (octet_length(cwd_label) <= 256),
  provider_updated_at timestamptz,
  status text NOT NULL CHECK (status IN ('idle', 'in_progress', 'waiting_approval', 'completed', 'failed', 'unknown')),
  last_sequence bigint NOT NULL DEFAULT 0 CHECK (last_sequence >= 0),
  created_at timestamptz NOT NULL DEFAULT statement_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT statement_timestamp(),
  UNIQUE (id, owner_id, installation_id),
  UNIQUE (owner_id, installation_id, profile_key, thread_id),
  FOREIGN KEY (installation_id, owner_id)
    REFERENCES client_installations(id, owner_id) ON DELETE CASCADE
);
CREATE INDEX codex_sessions_owner_installation ON codex_sessions(owner_id, installation_id, id);

CREATE TABLE codex_session_grants (
  session_id text PRIMARY KEY,
  owner_id text NOT NULL,
  installation_id text NOT NULL,
  scopes text[] NOT NULL DEFAULT '{}'::text[] CHECK (
    cardinality(scopes) <= 5 AND scopes <@ ARRAY['read','live','send','interrupt','approval']::text[]
  ),
  grant_version bigint NOT NULL DEFAULT 1 CHECK (grant_version >= 1),
  revoked_at timestamptz,
  updated_at timestamptz NOT NULL DEFAULT statement_timestamp(),
  FOREIGN KEY (session_id, owner_id, installation_id)
    REFERENCES codex_sessions(id, owner_id, installation_id) ON DELETE CASCADE
);
CREATE INDEX codex_session_grants_owner_installation ON codex_session_grants(owner_id, installation_id, session_id)
  WHERE revoked_at IS NULL;

CREATE TABLE codex_commands (
  id text PRIMARY KEY CHECK (id ~ '^g[a-z0-9]{7}$'),
  session_id text NOT NULL,
  owner_id text NOT NULL,
  installation_id text NOT NULL,
  idempotency_key text NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9._~-]{8,128}$'),
  request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
  type text NOT NULL CHECK (type IN ('send', 'interrupt', 'approval')),
  status text NOT NULL CHECK (status IN ('waiting_pc', 'claimed', 'forwarded', 'running', 'waiting_approval', 'completed', 'failed', 'unknown')),
  payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
  grant_version bigint NOT NULL CHECK (grant_version >= 1),
  status_version bigint NOT NULL DEFAULT 1 CHECK (status_version >= 1),
  result_code text NOT NULL DEFAULT '' CHECK (octet_length(result_code) <= 128),
  provider_request_id text NOT NULL DEFAULT '' CHECK (octet_length(provider_request_id) <= 512),
  created_at timestamptz NOT NULL DEFAULT statement_timestamp(),
  claimed_at timestamptz,
  updated_at timestamptz NOT NULL DEFAULT statement_timestamp(),
  UNIQUE (owner_id, session_id, idempotency_key),
  UNIQUE (id, session_id, owner_id, installation_id),
  FOREIGN KEY (session_id, owner_id, installation_id)
    REFERENCES codex_sessions(id, owner_id, installation_id) ON DELETE CASCADE
);
CREATE INDEX codex_commands_claimable ON codex_commands(owner_id, installation_id, created_at, id)
  WHERE status = 'waiting_pc';

CREATE TABLE codex_events (
  session_id text NOT NULL,
  owner_id text NOT NULL,
  installation_id text NOT NULL,
  sequence bigint NOT NULL CHECK (sequence > 0),
  provider_event_id text NOT NULL CHECK (octet_length(provider_event_id) BETWEEN 1 AND 512),
  kind text NOT NULL CHECK (kind IN (
    'user_message', 'assistant_message', 'assistant_delta', 'reasoning_summary', 'plan',
    'turn_started', 'turn_completed', 'turn_failed', 'turn_cancelled', 'tool_call',
    'tool_result', 'tool_call_summary', 'tool_result_summary', 'command_summary', 'file_change',
    'file_change_summary', 'child_agent', 'subagent_summary', 'question', 'approval_request',
    'approval_status', 'command_status', 'unsupported'
  )),
  occurred_at timestamptz NOT NULL,
  turn_id text NOT NULL DEFAULT '' CHECK (octet_length(turn_id) <= 512),
  item_id text NOT NULL DEFAULT '' CHECK (octet_length(item_id) <= 512),
  parent_item_id text NOT NULL DEFAULT '' CHECK (octet_length(parent_item_id) <= 512),
  status text NOT NULL DEFAULT '' CHECK (octet_length(status) <= 128),
  payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
  stored_at timestamptz NOT NULL DEFAULT statement_timestamp(),
  PRIMARY KEY (session_id, sequence),
  UNIQUE (session_id, provider_event_id),
  FOREIGN KEY (session_id, owner_id, installation_id)
    REFERENCES codex_sessions(id, owner_id, installation_id) ON DELETE CASCADE
);
CREATE INDEX codex_events_owner_session_sequence ON codex_events(owner_id, session_id, sequence);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM codex_sessions) THEN
    RAISE EXCEPTION 'cannot remove persisted Codex session and event history';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE codex_events;
DROP TABLE codex_commands;
DROP TABLE codex_session_grants;
DROP TABLE codex_sessions;
