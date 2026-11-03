-- +goose Up
ALTER TABLE conversations ADD COLUMN archived_at timestamptz;

CREATE INDEX conversations_owner_unarchived_created
  ON conversations(owner_id, created_at, id)
  WHERE archived_at IS NULL;

CREATE TABLE account_main_conversations (
  service_instance_id uuid NOT NULL REFERENCES service_identity(instance_id) ON DELETE RESTRICT,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  conversation_id text NOT NULL,
  version bigint NOT NULL CHECK (version >= 1),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (service_instance_id, owner_id),
  FOREIGN KEY (conversation_id, owner_id) REFERENCES conversations(id, owner_id)
);

-- Serialize every Conversation insert with per-owner main-conversation selection.
-- +goose StatementBegin
CREATE FUNCTION lock_conversation_owner_before_insert() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  PERFORM id FROM users WHERE id = NEW.owner_id FOR UPDATE;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER conversations_lock_owner_before_insert
BEFORE INSERT ON conversations
FOR EACH ROW EXECUTE FUNCTION lock_conversation_owner_before_insert();

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM account_main_conversations) THEN
    RAISE EXCEPTION 'cannot roll back while main conversation pointers are stored';
  END IF;
  IF EXISTS (SELECT 1 FROM conversations WHERE archived_at IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot remove archive markers while archived conversations exist';
  END IF;
END;
$$;
-- +goose StatementEnd

DROP TRIGGER conversations_lock_owner_before_insert ON conversations;
DROP FUNCTION lock_conversation_owner_before_insert();
DROP TABLE account_main_conversations;
DROP INDEX conversations_owner_unarchived_created;
ALTER TABLE conversations DROP COLUMN archived_at;
