-- +goose Up
-- The old API writer must be stopped before migration; these locks also fence
-- transactions already writing either source during the copy.
LOCK TABLE instant_chat_conversations, instant_chat_exchanges, conversations, conversation_messages, idempotency_keys
  IN ACCESS EXCLUSIVE MODE;

-- Fail before copying if two existing systems used the same identifier.
-- Even matching identifiers do not prove that two conversations are the same.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM instant_chat_conversations i JOIN conversations c ON c.id = i.id) THEN
    RAISE EXCEPTION 'instant chat conversation id collides with existing task conversation';
  END IF;
  IF EXISTS (
    SELECT 1 FROM instant_chat_exchanges i JOIN conversation_messages m
      ON m.id IN (i.user_message_id, i.assistant_message_id)
  ) OR EXISTS (
    SELECT message_id FROM (
      SELECT user_message_id AS message_id FROM instant_chat_exchanges
      UNION ALL SELECT assistant_message_id FROM instant_chat_exchanges
    ) AS ids GROUP BY message_id HAVING count(*) > 1
  ) THEN
    RAISE EXCEPTION 'instant chat message id collides with an existing message';
  END IF;
  IF EXISTS (
    SELECT 1 FROM instant_chat_exchanges i JOIN idempotency_keys k
      ON k.owner_id = i.owner_id AND k.operation = 'instant_chat.send' AND k.key = i.request_key
  ) THEN
    RAISE EXCEPTION 'instant chat request key collides with an existing idempotency record';
  END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE conversations ALTER COLUMN host_id DROP NOT NULL;
ALTER TABLE conversations
  ADD COLUMN model_id text NOT NULL DEFAULT '',
  ADD COLUMN source_kind text NOT NULL DEFAULT 'task',
  ADD COLUMN message_seq bigint NOT NULL DEFAULT 0 CHECK (message_seq >= 0);

ALTER TABLE tasks DROP CONSTRAINT tasks_conversation_id_owner_id_host_id_fkey;
ALTER TABLE tasks ALTER COLUMN host_id DROP NOT NULL;
ALTER TABLE tasks ADD CONSTRAINT tasks_conversation_owner_fkey
  FOREIGN KEY (conversation_id, owner_id) REFERENCES conversations(id, owner_id);
ALTER TABLE client_installations ADD CONSTRAINT client_installations_id_owner_unique UNIQUE (id, owner_id);
ALTER TABLE tasks DROP CONSTRAINT tasks_client_installation_snapshot_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_client_installation_snapshot_check CHECK (
  (auth_kind = 'client' AND auth_client_installation_id IS NOT NULL)
  OR auth_kind = 'session'
  OR (auth_kind NOT IN ('client', 'session') AND auth_client_installation_id IS NULL)
);
ALTER TABLE tasks ADD CONSTRAINT tasks_client_installation_owner_fkey
  FOREIGN KEY (auth_client_installation_id, owner_id) REFERENCES client_installations(id, owner_id);
ALTER TABLE tasks
  ADD COLUMN execution_location text NOT NULL DEFAULT 'legacy' CHECK (execution_location IN ('legacy', 'server')),
  ADD COLUMN workspace_id text,
  ADD COLUMN input_revision bigint,
  ADD COLUMN context_digest bytea CHECK (context_digest IS NULL OR octet_length(context_digest) = 32),
  ADD COLUMN auth_version bigint NOT NULL DEFAULT 1 CHECK (auth_version > 0),
  ADD COLUMN auth_expires_at timestamptz,
  ADD COLUMN agent_instance_id text NOT NULL DEFAULT '',
  ADD COLUMN agent_generation bigint NOT NULL DEFAULT 0 CHECK (agent_generation >= 0),
  ADD COLUMN agent_session_id text NOT NULL DEFAULT '',
  ADD COLUMN agent_engine_version text NOT NULL DEFAULT '',
  ADD COLUMN agent_profile_version text NOT NULL DEFAULT '',
  ADD COLUMN checkpoint_digest bytea CHECK (checkpoint_digest IS NULL OR octet_length(checkpoint_digest) = 32),
  ADD COLUMN checkpoint_through_seq bigint NOT NULL DEFAULT -1 CHECK (checkpoint_through_seq >= -1),
  ADD CONSTRAINT tasks_input_revision_pair_check CHECK ((workspace_id IS NULL) = (input_revision IS NULL)),
  ADD CONSTRAINT tasks_agent_identity_check CHECK (
    (agent_instance_id = '' AND agent_generation = 0)
    OR (agent_instance_id <> '' AND agent_generation > 0 AND agent_instance_id <> COALESCE(host_id, ''))
  ),
  ADD CONSTRAINT tasks_workspace_owner_fkey FOREIGN KEY (workspace_id, owner_id) REFERENCES workspaces(id, owner_id),
  ADD CONSTRAINT tasks_input_revision_fkey FOREIGN KEY (workspace_id, input_revision) REFERENCES workspace_revisions(workspace_id, revision);

CREATE TABLE task_agent_checkpoints (
  task_id text NOT NULL,
  owner_id text NOT NULL,
  agent_instance_id text NOT NULL CHECK (agent_instance_id <> ''),
  agent_generation bigint NOT NULL CHECK (agent_generation > 0),
  agent_session_id text NOT NULL CHECK (agent_session_id <> ''),
  from_seq bigint NOT NULL CHECK (from_seq >= 0),
  through_seq bigint NOT NULL CHECK (through_seq >= from_seq),
  digest bytea NOT NULL CHECK (octet_length(digest) = 32),
  ciphertext bytea NOT NULL CHECK (octet_length(ciphertext) BETWEEN 1 AND 16777216),
  engine_version text NOT NULL CHECK (engine_version <> ''),
  profile_version text NOT NULL CHECK (profile_version <> ''),
  auth_version bigint NOT NULL CHECK (auth_version > 0),
  lease_version bigint NOT NULL CHECK (lease_version > 0),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (task_id, agent_session_id, through_seq),
  UNIQUE (task_id, agent_session_id, from_seq),
  FOREIGN KEY (task_id, owner_id) REFERENCES tasks(id, owner_id) ON DELETE CASCADE
);

ALTER TABLE conversation_messages ALTER COLUMN task_id DROP NOT NULL;
ALTER TABLE conversation_messages
  ADD COLUMN model_id text NOT NULL DEFAULT '',
  ADD COLUMN source_kind text NOT NULL DEFAULT 'task',
  ADD COLUMN source_exchange_id text NOT NULL DEFAULT '',
  ADD COLUMN requested_conversation_id text NOT NULL DEFAULT '',
  ADD COLUMN sequence bigint;

INSERT INTO conversations (id, owner_id, host_id, title, created_at, updated_at, model_id, source_kind)
SELECT id, owner_id, NULL, title, created_at, updated_at, model_id, 'legacy_instant'
FROM instant_chat_conversations;

INSERT INTO conversation_messages
  (id, conversation_id, owner_id, role, content, task_id, created_at, model_id, source_kind, source_exchange_id, requested_conversation_id)
SELECT i.user_message_id, i.conversation_id, i.owner_id, 'user', i.user_content, NULL, i.created_at,
       c.model_id, 'legacy_instant', i.id, i.requested_conversation_id
FROM instant_chat_exchanges i JOIN instant_chat_conversations c ON c.id = i.conversation_id AND c.owner_id = i.owner_id
UNION ALL
SELECT i.assistant_message_id, i.conversation_id, i.owner_id, 'assistant', i.assistant_content, NULL, i.created_at,
       c.model_id, 'legacy_instant', i.id, i.requested_conversation_id
FROM instant_chat_exchanges i JOIN instant_chat_conversations c ON c.id = i.conversation_id AND c.owner_id = i.owner_id;

WITH ordered AS (
  SELECT id, row_number() OVER (
    PARTITION BY conversation_id
    ORDER BY created_at, CASE WHEN source_exchange_id <> '' THEN source_exchange_id ELSE id END,
      CASE role WHEN 'user' THEN 0 ELSE 1 END, id
  ) AS sequence
  FROM conversation_messages
)
UPDATE conversation_messages m SET sequence = ordered.sequence FROM ordered WHERE m.id = ordered.id;

UPDATE conversations c SET message_seq = COALESCE((
  SELECT max(m.sequence) FROM conversation_messages m WHERE m.conversation_id = c.id
), 0);
ALTER TABLE conversation_messages ALTER COLUMN sequence SET NOT NULL;
ALTER TABLE conversation_messages ADD CONSTRAINT conversation_messages_sequence_check CHECK (sequence > 0);
CREATE UNIQUE INDEX conversation_messages_sequence ON conversation_messages(conversation_id, sequence);

INSERT INTO idempotency_keys (owner_id, operation, key, request_hash, resource_id, related_id, created_at)
SELECT i.owner_id, 'instant_chat.send', i.request_key,
       sha256(convert_to(i.user_content || E'\n' || c.model_id || E'\n' || i.requested_conversation_id, 'UTF8')),
       i.user_message_id, i.assistant_message_id, i.created_at
FROM instant_chat_exchanges i JOIN instant_chat_conversations c ON c.id = i.conversation_id AND c.owner_id = i.owner_id;

-- Old binaries fail closed on their former table names instead of writing a
-- second history after the cutover; the original rows remain in place.
ALTER TABLE instant_chat_exchanges RENAME TO retired_instant_chat_exchanges;
ALTER TABLE instant_chat_conversations RENAME TO retired_instant_chat_conversations;

-- +goose Down
-- Downgrade is safe only before any new-format data has been created or old
-- copied data changed. Do not silently discard data produced after upgrade.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM tasks t JOIN conversations c ON c.id = t.conversation_id
             WHERE t.execution_location <> 'legacy' OR t.host_id IS NULL OR c.host_id IS DISTINCT FROM t.host_id
               OR (t.auth_kind = 'session' AND t.auth_client_installation_id IS NOT NULL))
     OR EXISTS (SELECT 1 FROM task_agent_checkpoints)
     OR EXISTS (SELECT 1 FROM conversations WHERE host_id IS NULL AND source_kind <> 'legacy_instant')
     OR EXISTS (SELECT 1 FROM conversation_messages WHERE task_id IS NULL AND source_kind <> 'legacy_instant')
     OR EXISTS (
       SELECT 1 FROM retired_instant_chat_conversations i
       LEFT JOIN conversations c ON c.id = i.id AND c.owner_id = i.owner_id
       WHERE c.id IS NULL OR c.source_kind <> 'legacy_instant' OR c.host_id IS NOT NULL
         OR c.title <> i.title OR c.model_id <> i.model_id OR c.created_at <> i.created_at OR c.updated_at <> i.updated_at
     )
     OR EXISTS (
       SELECT 1 FROM retired_instant_chat_exchanges i
       LEFT JOIN conversation_messages u ON u.id = i.user_message_id AND u.owner_id = i.owner_id
       LEFT JOIN conversation_messages a ON a.id = i.assistant_message_id AND a.owner_id = i.owner_id
       WHERE u.id IS NULL OR a.id IS NULL OR u.content <> i.user_content OR a.content <> i.assistant_content
         OR u.conversation_id <> i.conversation_id OR a.conversation_id <> i.conversation_id
         OR u.role <> 'user' OR a.role <> 'assistant' OR u.source_kind <> 'legacy_instant' OR a.source_kind <> 'legacy_instant'
         OR u.source_exchange_id <> i.id OR a.source_exchange_id <> i.id
         OR u.requested_conversation_id <> i.requested_conversation_id OR a.requested_conversation_id <> i.requested_conversation_id
     ) THEN
    RAISE EXCEPTION 'cannot roll back fused conversations while new or changed data exists';
  END IF;
END;
$$;
-- +goose StatementEnd
DELETE FROM idempotency_keys WHERE operation = 'instant_chat.send';
DELETE FROM conversation_messages WHERE source_kind = 'legacy_instant';
DELETE FROM conversations WHERE source_kind = 'legacy_instant';
DROP TABLE task_agent_checkpoints;
DROP INDEX conversation_messages_sequence;
ALTER TABLE conversation_messages DROP CONSTRAINT conversation_messages_sequence_check;
ALTER TABLE conversation_messages
  DROP COLUMN sequence, DROP COLUMN requested_conversation_id, DROP COLUMN source_exchange_id,
  DROP COLUMN source_kind, DROP COLUMN model_id;
ALTER TABLE conversation_messages ALTER COLUMN task_id SET NOT NULL;
ALTER TABLE tasks DROP CONSTRAINT tasks_input_revision_fkey;
ALTER TABLE tasks DROP CONSTRAINT tasks_workspace_owner_fkey;
ALTER TABLE tasks DROP CONSTRAINT tasks_agent_identity_check;
ALTER TABLE tasks DROP CONSTRAINT tasks_input_revision_pair_check;
ALTER TABLE tasks DROP CONSTRAINT tasks_client_installation_owner_fkey;
ALTER TABLE tasks DROP CONSTRAINT tasks_client_installation_snapshot_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_client_installation_snapshot_check CHECK (
  (auth_kind = 'client' AND auth_client_installation_id IS NOT NULL)
  OR (auth_kind <> 'client' AND auth_client_installation_id IS NULL)
);
ALTER TABLE client_installations DROP CONSTRAINT client_installations_id_owner_unique;
ALTER TABLE tasks
  DROP COLUMN checkpoint_through_seq, DROP COLUMN checkpoint_digest, DROP COLUMN agent_profile_version, DROP COLUMN agent_engine_version,
  DROP COLUMN agent_session_id, DROP COLUMN agent_generation, DROP COLUMN agent_instance_id,
  DROP COLUMN auth_expires_at, DROP COLUMN auth_version, DROP COLUMN context_digest,
  DROP COLUMN input_revision, DROP COLUMN workspace_id, DROP COLUMN execution_location;
ALTER TABLE tasks ALTER COLUMN host_id SET NOT NULL;
ALTER TABLE tasks DROP CONSTRAINT tasks_conversation_owner_fkey;
ALTER TABLE tasks ADD CONSTRAINT tasks_conversation_id_owner_id_host_id_fkey
  FOREIGN KEY (conversation_id, owner_id, host_id) REFERENCES conversations(id, owner_id, host_id);
ALTER TABLE conversations DROP COLUMN message_seq, DROP COLUMN source_kind, DROP COLUMN model_id;
ALTER TABLE conversations ALTER COLUMN host_id SET NOT NULL;
ALTER TABLE retired_instant_chat_conversations RENAME TO instant_chat_conversations;
ALTER TABLE retired_instant_chat_exchanges RENAME TO instant_chat_exchanges;
