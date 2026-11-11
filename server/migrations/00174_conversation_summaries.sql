-- +goose Up
-- Conversation summaries are actor-scoped derived data. Original messages
-- remain authoritative and are never rewritten by this migration.
CREATE TABLE conversation_summary_heads (
  service_instance_id uuid NOT NULL REFERENCES service_identity(instance_id) ON DELETE RESTRICT,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  actor_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  conversation_id text NOT NULL,
  scope_kind text NOT NULL CHECK (scope_kind IN ('personal', 'project')),
  scope_id text NOT NULL,
  authorization_version bigint NOT NULL CHECK (authorization_version > 0),
  scope_version bigint NOT NULL CHECK (scope_version >= 0),
  head_version bigint NOT NULL DEFAULT 0 CHECK (head_version >= 0),
  head_digest bytea NOT NULL DEFAULT '' CHECK (octet_length(head_digest) IN (0, 32)),
  lease_operation_id text NOT NULL DEFAULT '',
  lease_owner text NOT NULL DEFAULT '',
  lease_version bigint NOT NULL DEFAULT 0 CHECK (lease_version >= 0),
  lease_expires_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (service_instance_id, owner_id, actor_id, conversation_id, scope_kind, scope_id),
  FOREIGN KEY (conversation_id, owner_id) REFERENCES conversations(id, owner_id) ON DELETE RESTRICT,
  CHECK ((scope_kind = 'personal' AND scope_id = '' AND actor_id = owner_id AND scope_version = 0) OR
         (scope_kind = 'project' AND scope_id ~ '^j[a-z0-9]{7}$' AND scope_version > 0)),
  CHECK ((head_version = 0) = (octet_length(head_digest) = 0)),
  CHECK ((lease_operation_id = '') = (lease_owner = '' AND lease_expires_at IS NULL)),
  CHECK ((lease_owner = '') = (lease_expires_at IS NULL))
);

CREATE TABLE conversation_compaction_operations (
  service_instance_id uuid NOT NULL,
  owner_id text NOT NULL,
  actor_id text NOT NULL,
  conversation_id text NOT NULL,
  scope_kind text NOT NULL,
  scope_id text NOT NULL,
  operation_id text NOT NULL CHECK (operation_id ~ '^o[a-z0-9]{7}$'),
  authorization_version bigint NOT NULL CHECK (authorization_version > 0),
  scope_version bigint NOT NULL CHECK (scope_version >= 0),
  baseline_version bigint NOT NULL CHECK (baseline_version >= 0),
  input_digest bytea NOT NULL CHECK (octet_length(input_digest) = 32),
  state text NOT NULL CHECK (state IN ('claimed', 'checkpointed', 'unknown', 'committed', 'failed', 'cancelled', 'expired', 'invalidated')),
  lease_owner text NOT NULL DEFAULT '',
  lease_version bigint NOT NULL DEFAULT 0 CHECK (lease_version >= 0),
  lease_expires_at timestamptz,
  checkpoint_session_id text NOT NULL DEFAULT '',
  checkpoint_through_sequence bigint NOT NULL DEFAULT -1 CHECK (checkpoint_through_sequence >= -1),
  checkpoint_digest bytea NOT NULL DEFAULT '' CHECK (octet_length(checkpoint_digest) IN (0, 32)),
  model_call_id text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (service_instance_id, owner_id, actor_id, conversation_id, scope_kind, scope_id, operation_id),
  FOREIGN KEY (service_instance_id, owner_id, actor_id, conversation_id, scope_kind, scope_id)
    REFERENCES conversation_summary_heads(service_instance_id, owner_id, actor_id, conversation_id, scope_kind, scope_id) ON DELETE RESTRICT,
  CHECK ((scope_kind = 'personal' AND scope_id = '' AND actor_id = owner_id AND scope_version = 0) OR
         (scope_kind = 'project' AND scope_id ~ '^j[a-z0-9]{7}$' AND scope_version > 0)),
  CHECK ((checkpoint_through_sequence = -1) = (octet_length(checkpoint_digest) = 0)),
  CHECK ((checkpoint_through_sequence = -1) = (checkpoint_session_id = '')),
  CHECK ((lease_owner = '') = (lease_expires_at IS NULL))
);

CREATE UNIQUE INDEX conversation_compaction_one_live_operation
  ON conversation_compaction_operations(service_instance_id, owner_id, actor_id, conversation_id, scope_kind, scope_id)
  WHERE state IN ('claimed', 'checkpointed', 'unknown');

CREATE TABLE conversation_summary_versions (
  service_instance_id uuid NOT NULL,
  owner_id text NOT NULL,
  actor_id text NOT NULL,
  conversation_id text NOT NULL,
  scope_kind text NOT NULL,
  scope_id text NOT NULL,
  version bigint NOT NULL CHECK (version > 0),
  previous_version bigint CHECK (previous_version IS NULL OR previous_version > 0),
  operation_id text NOT NULL,
  authorization_version bigint NOT NULL CHECK (authorization_version > 0),
  scope_version bigint NOT NULL CHECK (scope_version >= 0),
  summary_digest bytea NOT NULL CHECK (octet_length(summary_digest) = 32),
  source_digest bytea NOT NULL CHECK (octet_length(source_digest) = 32),
  ciphertext bytea NOT NULL CHECK (octet_length(ciphertext) BETWEEN 1 AND 1048576),
  state text NOT NULL DEFAULT 'valid' CHECK (state IN ('valid', 'invalid')),
  invalid_reason text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  invalidated_at timestamptz,
  PRIMARY KEY (service_instance_id, owner_id, actor_id, conversation_id, scope_kind, scope_id, version),
  UNIQUE (service_instance_id, owner_id, actor_id, conversation_id, scope_kind, scope_id, operation_id),
  FOREIGN KEY (service_instance_id, owner_id, actor_id, conversation_id, scope_kind, scope_id)
    REFERENCES conversation_summary_heads(service_instance_id, owner_id, actor_id, conversation_id, scope_kind, scope_id) ON DELETE RESTRICT,
  FOREIGN KEY (service_instance_id, owner_id, actor_id, conversation_id, scope_kind, scope_id, operation_id)
    REFERENCES conversation_compaction_operations(service_instance_id, owner_id, actor_id, conversation_id, scope_kind, scope_id, operation_id) ON DELETE RESTRICT,
  FOREIGN KEY (service_instance_id, owner_id, actor_id, conversation_id, scope_kind, scope_id, previous_version)
    REFERENCES conversation_summary_versions(service_instance_id, owner_id, actor_id, conversation_id, scope_kind, scope_id, version) ON DELETE RESTRICT,
  CHECK (previous_version IS NULL OR previous_version < version),
  CHECK ((state = 'invalid') = (invalidated_at IS NOT NULL)),
  CHECK ((state = 'invalid') = (invalid_reason <> '')),
  CHECK ((scope_kind = 'personal' AND scope_id = '' AND actor_id = owner_id AND scope_version = 0) OR
         (scope_kind = 'project' AND scope_id ~ '^j[a-z0-9]{7}$' AND scope_version > 0))
);

CREATE INDEX conversation_summary_versions_valid_head
  ON conversation_summary_versions(service_instance_id, owner_id, actor_id, conversation_id, scope_kind, scope_id, version DESC)
  WHERE state = 'valid';

CREATE TABLE conversation_summary_sources (
  service_instance_id uuid NOT NULL,
  owner_id text NOT NULL,
  actor_id text NOT NULL,
  conversation_id text NOT NULL,
  scope_kind text NOT NULL,
  scope_id text NOT NULL,
  summary_version bigint NOT NULL,
  source_order integer NOT NULL CHECK (source_order > 0),
  source_kind text NOT NULL CHECK (source_kind IN ('message', 'task_result', 'memory')),
  source_id text NOT NULL CHECK (length(source_id) BETWEEN 1 AND 128),
  source_object_type text NOT NULL DEFAULT '',
  source_version bigint NOT NULL CHECK (source_version >= 0),
  source_sequence bigint NOT NULL CHECK (source_sequence >= 0),
  source_digest bytea NOT NULL CHECK (octet_length(source_digest) = 32),
  PRIMARY KEY (service_instance_id, owner_id, actor_id, conversation_id, scope_kind, scope_id, summary_version, source_order),
  UNIQUE (service_instance_id, owner_id, actor_id, conversation_id, scope_kind, scope_id, summary_version,
          source_kind, source_id, source_version, source_sequence),
  FOREIGN KEY (service_instance_id, owner_id, actor_id, conversation_id, scope_kind, scope_id, summary_version)
    REFERENCES conversation_summary_versions(service_instance_id, owner_id, actor_id, conversation_id, scope_kind, scope_id, version) ON DELETE RESTRICT,
  CHECK ((source_kind = 'memory' AND source_object_type <> '') OR (source_kind <> 'memory' AND source_object_type = ''))
);

CREATE INDEX conversation_summary_sources_lookup
  ON conversation_summary_sources(owner_id, source_kind, source_id, source_version, source_sequence);

-- +goose Down
-- Summary/source and operation records are recovery and authorization evidence.
-- Do not drop them automatically during rollback.
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM conversation_summary_sources)
     OR EXISTS (SELECT 1 FROM conversation_summary_versions)
     OR EXISTS (SELECT 1 FROM conversation_compaction_operations)
     OR EXISTS (SELECT 1 FROM conversation_summary_heads) THEN
    RAISE EXCEPTION 'cannot remove persisted conversation summaries or compaction operations';
  END IF;
END $$;
-- +goose StatementEnd
DROP INDEX conversation_summary_sources_lookup;
DROP TABLE conversation_summary_sources;
DROP INDEX conversation_summary_versions_valid_head;
DROP TABLE conversation_summary_versions;
DROP INDEX conversation_compaction_one_live_operation;
DROP TABLE conversation_compaction_operations;
DROP TABLE conversation_summary_heads;
