-- +goose Up
CREATE TABLE instant_chat_conversations (
  id text PRIMARY KEY,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  model_id text NOT NULL,
  title text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (id, owner_id)
);
CREATE INDEX instant_chat_conversations_owner_updated
  ON instant_chat_conversations(owner_id, updated_at DESC, id DESC);

CREATE TABLE instant_chat_exchanges (
  id text PRIMARY KEY,
  conversation_id text NOT NULL,
  owner_id text NOT NULL,
  request_key text NOT NULL,
  user_message_id text NOT NULL UNIQUE,
  assistant_message_id text NOT NULL UNIQUE,
  user_content text NOT NULL,
  assistant_content text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (owner_id, request_key),
  FOREIGN KEY (conversation_id, owner_id)
    REFERENCES instant_chat_conversations(id, owner_id) ON DELETE CASCADE
);
CREATE INDEX instant_chat_exchanges_conversation_created
  ON instant_chat_exchanges(owner_id, conversation_id, created_at, id);

-- +goose Down
DROP TABLE instant_chat_exchanges;
DROP TABLE instant_chat_conversations;
