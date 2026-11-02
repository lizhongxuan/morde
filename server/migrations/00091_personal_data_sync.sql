-- +goose Up
LOCK TABLE conversations, conversation_messages IN SHARE ROW EXCLUSIVE MODE;

CREATE TABLE account_change_counters (
  service_instance_id uuid NOT NULL REFERENCES service_identity(instance_id) ON DELETE RESTRICT,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  last_sequence bigint NOT NULL DEFAULT 0 CHECK (last_sequence >= 0),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (service_instance_id, owner_id)
);

CREATE TABLE account_object_versions (
  service_instance_id uuid NOT NULL REFERENCES service_identity(instance_id) ON DELETE RESTRICT,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  object_type text NOT NULL CHECK (octet_length(object_type) BETWEEN 1 AND 64),
  object_id text NOT NULL CHECK (octet_length(object_id) BETWEEN 1 AND 128),
  revision bigint NOT NULL CHECK (revision > 0),
  is_deleted boolean NOT NULL DEFAULT false,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (service_instance_id, owner_id, object_type, object_id),
  FOREIGN KEY (service_instance_id, owner_id)
    REFERENCES account_change_counters(service_instance_id, owner_id) ON DELETE CASCADE
);

CREATE TABLE account_change_log (
  service_instance_id uuid NOT NULL REFERENCES service_identity(instance_id) ON DELETE RESTRICT,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  change_sequence bigint NOT NULL CHECK (change_sequence > 0),
  object_type text NOT NULL CHECK (octet_length(object_type) BETWEEN 1 AND 64),
  object_id text NOT NULL CHECK (octet_length(object_id) BETWEEN 1 AND 128),
  revision bigint NOT NULL CHECK (revision > 0),
  operation text NOT NULL CHECK (operation IN ('upsert', 'delete')),
  mutation_id text CHECK (mutation_id IS NULL OR octet_length(mutation_id) BETWEEN 8 AND 128),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (service_instance_id, owner_id, change_sequence),
  FOREIGN KEY (service_instance_id, owner_id)
    REFERENCES account_change_counters(service_instance_id, owner_id) ON DELETE CASCADE
);

CREATE TABLE personal_objects (
  service_instance_id uuid NOT NULL REFERENCES service_identity(instance_id) ON DELETE RESTRICT,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  object_type text NOT NULL CHECK (octet_length(object_type) BETWEEN 1 AND 64),
  object_id text NOT NULL CHECK (octet_length(object_id) BETWEEN 1 AND 128),
  body jsonb CHECK (body IS NULL OR jsonb_typeof(body) = 'object'),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (service_instance_id, owner_id, object_type, object_id)
);

CREATE TABLE personal_mutation_receipts (
  service_instance_id uuid NOT NULL REFERENCES service_identity(instance_id) ON DELETE RESTRICT,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  mutation_id text NOT NULL CHECK (octet_length(mutation_id) BETWEEN 8 AND 128),
  request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
  object_type text NOT NULL CHECK (octet_length(object_type) BETWEEN 1 AND 64),
  object_id text NOT NULL CHECK (octet_length(object_id) BETWEEN 1 AND 128),
  result_revision bigint NOT NULL CHECK (result_revision > 0),
  change_sequence bigint NOT NULL CHECK (change_sequence > 0),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (service_instance_id, owner_id, mutation_id)
);

INSERT INTO account_change_counters (service_instance_id, owner_id)
SELECT instance.instance_id, users.id
FROM service_identity AS instance
CROSS JOIN users
ON CONFLICT (service_instance_id, owner_id) DO NOTHING;

INSERT INTO account_object_versions (service_instance_id, owner_id, object_type, object_id, revision, is_deleted)
SELECT instance.instance_id, conversations.owner_id, 'conversation', conversations.id, 1, false
FROM conversations CROSS JOIN service_identity AS instance
ON CONFLICT (service_instance_id, owner_id, object_type, object_id) DO NOTHING;

INSERT INTO account_object_versions (service_instance_id, owner_id, object_type, object_id, revision, is_deleted)
SELECT instance.instance_id, messages.owner_id, 'message', messages.id, 1, false
FROM conversation_messages AS messages CROSS JOIN service_identity AS instance
ON CONFLICT (service_instance_id, owner_id, object_type, object_id) DO NOTHING;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM personal_mutation_receipts)
     OR EXISTS (SELECT 1 FROM personal_objects)
     OR EXISTS (SELECT 1 FROM account_change_log)
     OR EXISTS (SELECT 1 FROM account_object_versions)
     OR EXISTS (SELECT 1 FROM account_change_counters) THEN
    RAISE EXCEPTION 'cannot roll back personal data synchronization while account data exists';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE personal_mutation_receipts;
DROP TABLE personal_objects;
DROP TABLE account_change_log;
DROP TABLE account_object_versions;
DROP TABLE account_change_counters;
