-- +goose Up
ALTER TABLE conversations ADD COLUMN project_id text;
ALTER TABLE conversations
  ADD CONSTRAINT conversations_project_id_fkey
  FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE RESTRICT,
  ADD CONSTRAINT conversations_id_owner_project_unique UNIQUE (id, owner_id, project_id);

ALTER TABLE tasks ADD COLUMN project_id text;
ALTER TABLE tasks ADD COLUMN initiator_id text REFERENCES users(id) ON DELETE RESTRICT;
UPDATE tasks SET initiator_id = owner_id;
ALTER TABLE tasks ALTER COLUMN initiator_id SET NOT NULL;
ALTER TABLE tasks
  ADD CONSTRAINT tasks_project_id_fkey
  FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE RESTRICT,
  ADD CONSTRAINT tasks_conversation_project_fkey
  FOREIGN KEY (conversation_id, owner_id, project_id)
  REFERENCES conversations(id, owner_id, project_id);

ALTER TABLE conversation_messages ADD COLUMN actor_id text REFERENCES users(id) ON DELETE RESTRICT;
UPDATE conversation_messages SET actor_id = owner_id;
ALTER TABLE conversation_messages ALTER COLUMN actor_id SET NOT NULL;

CREATE INDEX conversations_project_unarchived_created
  ON conversations(project_id, created_at DESC, id DESC)
  WHERE project_id IS NOT NULL AND archived_at IS NULL;
CREATE INDEX tasks_project_created
  ON tasks(project_id, created_at DESC, id DESC)
  WHERE project_id IS NOT NULL;

-- Project history is readable only through current membership. These insert/update
-- guards prevent a writer from assigning a nonmember as the persisted actor.
-- They deliberately leave old rows self-owned and let membership revocation stand.
-- +goose StatementBegin
CREATE FUNCTION require_current_project_member(project_key text, actor_key text) RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
  PERFORM 1 FROM project_members
   WHERE project_id = project_key AND user_id = actor_key
   FOR KEY SHARE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'project actor is not a current member' USING ERRCODE = '23503';
  END IF;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION guard_conversation_project_scope() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.project_id IS NOT NULL THEN
    PERFORM require_current_project_member(NEW.project_id, NEW.owner_id);
  END IF;
  IF TG_OP = 'UPDATE' AND NEW.project_id IS DISTINCT FROM OLD.project_id
     AND EXISTS (SELECT 1 FROM tasks WHERE conversation_id = OLD.id AND owner_id = OLD.owner_id) THEN
    RAISE EXCEPTION 'conversation project scope cannot change after tasks exist' USING ERRCODE = '23503';
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER conversations_guard_project_scope
BEFORE INSERT OR UPDATE OF project_id, owner_id ON conversations
FOR EACH ROW EXECUTE FUNCTION guard_conversation_project_scope();

-- +goose StatementBegin
CREATE FUNCTION guard_task_project_scope() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  conversation_project_id text;
BEGIN
  -- Older personal writers omit initiator_id; preserve their owner attribution.
  IF TG_OP = 'INSERT' AND NEW.initiator_id IS NULL THEN
    NEW.initiator_id := NEW.owner_id;
  END IF;
  SELECT project_id INTO conversation_project_id FROM conversations
   WHERE id = NEW.conversation_id AND owner_id = NEW.owner_id;
  IF FOUND AND NEW.project_id IS DISTINCT FROM conversation_project_id THEN
    RAISE EXCEPTION 'task project scope must match its conversation' USING ERRCODE = '23503';
  END IF;
  IF NEW.project_id IS NOT NULL THEN
    PERFORM require_current_project_member(NEW.project_id, NEW.initiator_id);
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER tasks_guard_project_scope
BEFORE INSERT OR UPDATE OF project_id, initiator_id, conversation_id, owner_id ON tasks
FOR EACH ROW EXECUTE FUNCTION guard_task_project_scope();

-- +goose StatementBegin
CREATE FUNCTION guard_conversation_message_actor() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  conversation_project_id text;
BEGIN
  -- Older personal writers omit actor_id; preserve their owner attribution.
  IF TG_OP = 'INSERT' AND NEW.actor_id IS NULL THEN
    NEW.actor_id := NEW.owner_id;
  END IF;
  SELECT project_id INTO conversation_project_id FROM conversations
   WHERE id = NEW.conversation_id AND owner_id = NEW.owner_id;
  IF FOUND THEN
    IF conversation_project_id IS NULL THEN
      IF NEW.actor_id IS DISTINCT FROM NEW.owner_id THEN
        RAISE EXCEPTION 'personal message actor must be its owner' USING ERRCODE = '23503';
      END IF;
    ELSE
      PERFORM require_current_project_member(conversation_project_id, NEW.actor_id);
    END IF;
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER conversation_messages_guard_actor
BEFORE INSERT OR UPDATE OF actor_id, conversation_id, owner_id ON conversation_messages
FOR EACH ROW EXECUTE FUNCTION guard_conversation_message_actor();

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM conversations WHERE project_id IS NOT NULL)
     OR EXISTS (SELECT 1 FROM tasks WHERE project_id IS NOT NULL OR initiator_id <> owner_id)
     OR EXISTS (SELECT 1 FROM conversation_messages WHERE actor_id <> owner_id) THEN
    RAISE EXCEPTION 'cannot remove project scope while scoped history or non-owner actors exist';
  END IF;
END;
$$;
-- +goose StatementEnd

DROP TRIGGER conversation_messages_guard_actor ON conversation_messages;
DROP FUNCTION guard_conversation_message_actor();
DROP TRIGGER tasks_guard_project_scope ON tasks;
DROP FUNCTION guard_task_project_scope();
DROP TRIGGER conversations_guard_project_scope ON conversations;
DROP FUNCTION guard_conversation_project_scope();
DROP FUNCTION require_current_project_member(text, text);

DROP INDEX tasks_project_created;
DROP INDEX conversations_project_unarchived_created;

ALTER TABLE conversation_messages DROP COLUMN actor_id;
ALTER TABLE tasks
  DROP CONSTRAINT tasks_conversation_project_fkey,
  DROP CONSTRAINT tasks_project_id_fkey,
  DROP COLUMN initiator_id,
  DROP COLUMN project_id;
ALTER TABLE conversations
  DROP CONSTRAINT conversations_id_owner_project_unique,
  DROP CONSTRAINT conversations_project_id_fkey,
  DROP COLUMN project_id;
