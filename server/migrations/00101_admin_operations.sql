-- +goose Up
CREATE TABLE user_invite_codes (
  owner_id   text PRIMARY KEY REFERENCES users(id) ON DELETE RESTRICT,
  code       text NOT NULL UNIQUE CHECK (code ~ '^[a-f0-9]{32}$'),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (owner_id, code)
);

CREATE TABLE user_invites (
  inviter_id text NOT NULL,
  invitee_id text NOT NULL UNIQUE REFERENCES users(id) ON DELETE RESTRICT,
  invite_code text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (inviter_id, invitee_id),
  CONSTRAINT user_invites_distinct_accounts CHECK (inviter_id <> invitee_id),
  CONSTRAINT user_invites_code_owner_fk FOREIGN KEY (inviter_id, invite_code)
    REFERENCES user_invite_codes(owner_id, code) ON DELETE RESTRICT
);
CREATE INDEX user_invites_inviter_created ON user_invites(inviter_id, created_at DESC, invitee_id);

CREATE TABLE admin_nudge_policy (
  id         smallint PRIMARY KEY CHECK (id = 1),
  enabled    boolean NOT NULL DEFAULT false,
  url        text NOT NULL DEFAULT '',
  text_zh    text NOT NULL DEFAULT '',
  text_en    text NOT NULL DEFAULT '',
  version    bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  updated_at timestamptz
);
INSERT INTO admin_nudge_policy (id) VALUES (1);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM user_invites) OR EXISTS (SELECT 1 FROM user_invite_codes) OR
     EXISTS (SELECT 1 FROM admin_nudge_policy WHERE version > 1 OR enabled OR url <> '' OR text_zh <> '' OR text_en <> '') THEN
    RAISE EXCEPTION 'cannot roll back admin operation data';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE admin_nudge_policy;
DROP TABLE user_invites;
DROP TABLE user_invite_codes;
