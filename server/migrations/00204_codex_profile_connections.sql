-- +goose Up
-- A connection is the authenticated owner's grant ceiling for one Codex profile
-- on one PC installation. Session mirrors remain separately persisted so
-- revoking a profile can block access without deleting conversation history.
CREATE TABLE codex_profile_connections (
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  installation_id text NOT NULL,
  profile_key text NOT NULL CHECK (profile_key ~ '^[A-Za-z0-9_-]{1,128}$' AND profile_key !~* 'CODEX_HOME'),
  scopes text[] NOT NULL DEFAULT '{}'::text[] CHECK (
    cardinality(scopes) <= 5 AND scopes <@ ARRAY['read','live','send','interrupt','approval']::text[]
  ),
  grant_version bigint NOT NULL DEFAULT 1 CHECK (grant_version >= 1),
  revoked_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT statement_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT statement_timestamp(),
  PRIMARY KEY (owner_id, installation_id, profile_key),
  CHECK (revoked_at IS NOT NULL OR scopes @> ARRAY['read','live']::text[]),
  CHECK (revoked_at IS NULL OR cardinality(scopes)=0),
  FOREIGN KEY (installation_id, owner_id)
    REFERENCES client_installations(id, owner_id) ON DELETE CASCADE
);
CREATE INDEX codex_profile_connections_owner_installation
  ON codex_profile_connections(owner_id, installation_id, profile_key)
  WHERE revoked_at IS NULL;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM codex_profile_connections) THEN
    RAISE EXCEPTION 'cannot remove persisted Codex profile connection grants';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE codex_profile_connections;
