-- +goose Up
ALTER TABLE hosts
  ADD COLUMN runtime_generation bigint NOT NULL DEFAULT 0 CHECK (runtime_generation >= 0),
  ADD COLUMN control_version bigint NOT NULL DEFAULT 0 CHECK (control_version >= 0),
  ADD COLUMN control_state text NOT NULL DEFAULT 'ai'
    CHECK (control_state IN ('ai', 'taking_over', 'user', 'returning'));
ALTER TABLE hosts ADD CONSTRAINT hosts_id_owner_unique UNIQUE (id, owner_id);

CREATE TABLE conversations (
  id         text PRIMARY KEY,
  owner_id   text NOT NULL REFERENCES users(id),
  host_id    text NOT NULL,
  title      text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (id, owner_id),
  UNIQUE (id, owner_id, host_id),
  FOREIGN KEY (host_id, owner_id) REFERENCES hosts(id, owner_id)
);

CREATE TABLE tasks (
  id               text PRIMARY KEY,
  owner_id         text NOT NULL REFERENCES users(id),
  conversation_id  text NOT NULL,
  host_id          text NOT NULL,
  model_id         text NOT NULL,
  objective        text NOT NULL,
  status           text NOT NULL CHECK (status IN ('queued', 'running', 'waiting_user', 'pausing', 'paused', 'cancelling', 'completed', 'failed', 'cancelled')),
  auth_kind        text NOT NULL,
  auth_token_id    text NOT NULL DEFAULT '',
  auth_scopes      text[] NOT NULL DEFAULT '{}',
  budget_active_seconds integer NOT NULL CHECK (budget_active_seconds BETWEEN 1 AND 1800),
  budget_tool_calls     integer NOT NULL CHECK (budget_tool_calls BETWEEN 1 AND 100),
  budget_model_tokens   integer NOT NULL CHECK (budget_model_tokens BETWEEN 1 AND 50000),
  usage_active_seconds  bigint NOT NULL DEFAULT 0 CHECK (usage_active_seconds >= 0),
  usage_tool_calls      bigint NOT NULL DEFAULT 0 CHECK (usage_tool_calls >= 0),
  usage_model_tokens    bigint CHECK (usage_model_tokens IS NULL OR usage_model_tokens >= 0),
  lease_owner      text NOT NULL DEFAULT '',
  lease_version    bigint NOT NULL DEFAULT 0 CHECK (lease_version >= 0),
  lease_expires_at timestamptz,
  event_seq        bigint NOT NULL DEFAULT 0 CHECK (event_seq >= 0),
  current_step     text NOT NULL DEFAULT '',
  result           text NOT NULL DEFAULT '',
  waiting_reason   text NOT NULL DEFAULT '',
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  finished_at      timestamptz,
  UNIQUE (id, owner_id, conversation_id),
  UNIQUE (id, host_id),
  FOREIGN KEY (conversation_id, owner_id, host_id) REFERENCES conversations(id, owner_id, host_id),
  FOREIGN KEY (host_id, owner_id) REFERENCES hosts(id, owner_id),
  CHECK ((lease_owner = '') = (lease_expires_at IS NULL)),
  CHECK (status <> 'running' OR (lease_owner <> '' AND lease_expires_at IS NOT NULL))
);
CREATE INDEX tasks_owner_created ON tasks(owner_id, created_at DESC, id DESC);
CREATE INDEX tasks_host_created ON tasks(owner_id, host_id, created_at DESC, id DESC);
CREATE INDEX tasks_claimable ON tasks(status, lease_expires_at, created_at, id)
  WHERE status IN ('queued', 'running');
CREATE UNIQUE INDEX tasks_one_active_per_conversation ON tasks(conversation_id)
  WHERE status IN ('queued', 'running', 'waiting_user', 'pausing', 'cancelling');

CREATE TABLE conversation_messages (
  id              text PRIMARY KEY,
  conversation_id text NOT NULL,
  owner_id        text NOT NULL,
  role            text NOT NULL CHECK (role IN ('user', 'assistant')),
  content         text NOT NULL,
  task_id         text NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (conversation_id, owner_id) REFERENCES conversations(id, owner_id),
  FOREIGN KEY (task_id, owner_id, conversation_id) REFERENCES tasks(id, owner_id, conversation_id)
);
CREATE INDEX conversation_messages_conversation_created ON conversation_messages(owner_id, conversation_id, created_at DESC, id DESC);
CREATE INDEX conversation_messages_task ON conversation_messages(task_id) WHERE task_id IS NOT NULL;

CREATE TABLE task_steps (
  id         text PRIMARY KEY,
  task_id    text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  sequence   bigint NOT NULL CHECK (sequence > 0),
  kind       text NOT NULL,
  status     text NOT NULL CHECK (status IN ('queued', 'running', 'waiting_user', 'succeeded', 'failed', 'unknown')),
  title      text NOT NULL DEFAULT '',
  summary    text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (task_id, sequence),
  UNIQUE (task_id, id)
);

CREATE TABLE task_operations (
  id                 text PRIMARY KEY,
  task_id            text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  operation_key      text NOT NULL,
  request_hash       bytea NOT NULL CHECK (octet_length(request_hash) = 32),
  host_id            text NOT NULL,
  runtime_generation bigint NOT NULL CHECK (runtime_generation >= 0),
  control_version    bigint NOT NULL CHECK (control_version >= 0),
  lease_version      bigint NOT NULL CHECK (lease_version > 0),
  status             text NOT NULL CHECK (status IN ('prepared', 'dispatched', 'succeeded', 'failed', 'unknown')),
  external_id        text NOT NULL DEFAULT '',
  result             text NOT NULL DEFAULT '',
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  UNIQUE (task_id, operation_key),
  FOREIGN KEY (task_id, host_id) REFERENCES tasks(id, host_id)
);
CREATE INDEX task_operations_status ON task_operations(task_id, status);

CREATE TABLE task_batch_items (
  id         text PRIMARY KEY,
  task_id    text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  item_key   text NOT NULL,
  input      text NOT NULL,
  status     text NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'unknown')),
  result     text NOT NULL DEFAULT '',
  error      text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (task_id, item_key)
);

CREATE TABLE task_artifacts (
  id         text PRIMARY KEY,
  task_id    text NOT NULL,
  step_id    text NOT NULL,
  label      text NOT NULL,
  path       text NOT NULL,
  media_type text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (task_id, step_id) REFERENCES task_steps(task_id, id) ON DELETE CASCADE
);

CREATE TABLE task_events (
  task_id    text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  sequence   bigint NOT NULL CHECK (sequence > 0),
  event_type text NOT NULL,
  payload    jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (task_id, sequence)
);

CREATE TABLE idempotency_keys (
  owner_id     text NOT NULL REFERENCES users(id),
  operation    text NOT NULL,
  key          text NOT NULL,
  request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
  resource_id  text NOT NULL,
  related_id   text NOT NULL DEFAULT '',
  created_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (owner_id, operation, key)
);

-- +goose Down
DROP TABLE idempotency_keys;
DROP TABLE task_events;
DROP TABLE task_artifacts;
DROP TABLE task_batch_items;
DROP TABLE task_operations;
DROP TABLE task_steps;
DROP TABLE conversation_messages;
DROP TABLE tasks;
DROP TABLE conversations;
ALTER TABLE hosts DROP CONSTRAINT hosts_id_owner_unique;
ALTER TABLE hosts DROP COLUMN control_state, DROP COLUMN control_version, DROP COLUMN runtime_generation;
