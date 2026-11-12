-- +goose Up
CREATE TABLE host_embed_tickets (
  ticket_hash            bytea PRIMARY KEY CHECK (octet_length(ticket_hash) = 32),
  host_id                text NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
  conversation_id        text NOT NULL,
  actor_user_id          text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  portal_token_id        text NOT NULL REFERENCES tokens(id) ON DELETE CASCADE,
  client_installation_id text REFERENCES client_installations(id) ON DELETE CASCADE,
  service_instance_id    text NOT NULL,
  portal_origin          text NOT NULL,
  initial_target         text NOT NULL CHECK (initial_target IN ('browser', 'terminal', 'workspace', 'files', 'vscode')),
  target_grants          jsonb NOT NULL CHECK (jsonb_typeof(target_grants) = 'array' AND jsonb_array_length(target_grants) BETWEEN 1 AND 5),
  embed_nonce            text NOT NULL CHECK (embed_nonce ~ '^[A-Za-z0-9_-]{32}$'),
  runtime_generation     bigint NOT NULL CHECK (runtime_generation > 0),
  expires_at             timestamptz NOT NULL,
  used_at                timestamptz,
  created_at             timestamptz NOT NULL DEFAULT clock_timestamp(),
  FOREIGN KEY (conversation_id, actor_user_id) REFERENCES conversations(id, owner_id) ON DELETE CASCADE,
  CHECK (portal_origin ~ '^https?://[^/?#]+$')
);

CREATE INDEX host_embed_tickets_expiry ON host_embed_tickets(expires_at);

-- +goose Down
DROP TABLE host_embed_tickets;
