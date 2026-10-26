-- +goose Up
ALTER TABLE tokens ADD COLUMN actor_user_id text REFERENCES users(id);
ALTER TABLE tokens ADD CONSTRAINT tokens_actor_user_kind_check
  CHECK (actor_user_id IS NULL OR kind = 'ticket');
ALTER TABLE tokens ADD CONSTRAINT tokens_actor_user_owner_check
  CHECK (actor_user_id IS NULL OR actor_user_id <> owner_id);

CREATE TABLE host_members (
  host_id      text NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
  owner_id     text NOT NULL REFERENCES users(id),
  user_id      text NOT NULL REFERENCES users(id),
  permissions  text[] NOT NULL CHECK (cardinality(permissions) BETWEEN 1 AND 4),
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  revoked_at   timestamptz,
  PRIMARY KEY (host_id, user_id),
  CHECK (owner_id <> user_id),
  CHECK (permissions <@ ARRAY['desktop:view', 'desktop:control', 'terminal:control', 'mcp:control']::text[])
);
CREATE INDEX host_members_user_active ON host_members(user_id, host_id) WHERE revoked_at IS NULL;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM host_members) OR EXISTS (SELECT 1 FROM tokens WHERE actor_user_id IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot roll back Host sharing while member grants or actor tickets exist';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE host_members;
ALTER TABLE tokens DROP CONSTRAINT tokens_actor_user_owner_check;
ALTER TABLE tokens DROP CONSTRAINT tokens_actor_user_kind_check;
ALTER TABLE tokens DROP COLUMN actor_user_id;
