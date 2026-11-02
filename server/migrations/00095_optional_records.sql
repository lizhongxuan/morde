-- +goose Up
CREATE TABLE record_consents (
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  purpose text NOT NULL CHECK (purpose IN ('full_record','training_contribution')),
  enabled boolean NOT NULL DEFAULT false,
  version bigint NOT NULL CHECK (version >= 1),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (owner_id, purpose)
);

CREATE TABLE record_quotas (
  owner_id text PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  retention_days integer NOT NULL DEFAULT 30 CHECK (retention_days BETWEEN 1 AND 3650),
  limit_bytes bigint NOT NULL DEFAULT 1073741824 CHECK (limit_bytes BETWEEN 1 AND 1099511627776),
  used_bytes bigint NOT NULL DEFAULT 0 CHECK (used_bytes >= 0),
  reserved_bytes bigint NOT NULL DEFAULT 0 CHECK (reserved_bytes >= 0),
  version bigint NOT NULL DEFAULT 1 CHECK (version >= 1),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE record_admin_grants (
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  admin_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  capability text NOT NULL CHECK (capability IN ('view','attachment','export','delete')),
  granted boolean NOT NULL,
  granted_by text NOT NULL REFERENCES users(id),
  version bigint NOT NULL CHECK (version >= 1),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (owner_id, admin_id, capability),
  CHECK (owner_id <> admin_id)
);

CREATE TABLE record_capture_queue (
  id text PRIMARY KEY CHECK (id ~ '^j[a-z0-9]{7}$'),
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  purpose text NOT NULL CHECK (purpose IN ('full_record','training_contribution')),
  item_kind text NOT NULL CHECK (item_kind IN ('turn','sample')),
  consent_version bigint NOT NULL CHECK (consent_version >= 1),
  captured_by_installation text REFERENCES client_installations(id) ON DELETE SET NULL,
  captured_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL,
  payload bytea NOT NULL CHECK (octet_length(payload) BETWEEN 1 AND 33554432),
  reserved_bytes bigint NOT NULL CHECK (reserved_bytes > 0),
  state text NOT NULL CHECK (state IN ('queued','leased','stored','rejected','failed')),
  lease_owner text,
  lease_expires_at timestamptz,
  lease_version bigint NOT NULL DEFAULT 0 CHECK (lease_version >= 0),
  attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  failure_code text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  CHECK (expires_at > captured_at),
  CHECK ((state = 'leased') = (lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL))
);

CREATE INDEX record_capture_queue_ready_idx ON record_capture_queue (created_at, id)
  WHERE state IN ('queued','leased');
CREATE INDEX record_capture_queue_owner_idx ON record_capture_queue (owner_id, purpose, state);

CREATE TABLE record_capture_queue_attachments (
  id text PRIMARY KEY CHECK (id ~ '^g[a-z0-9]{7}$'),
  queue_item_id text NOT NULL REFERENCES record_capture_queue(id) ON DELETE CASCADE,
  file_name text NOT NULL CHECK (length(file_name) BETWEEN 1 AND 255),
  media_type text NOT NULL CHECK (length(media_type) BETWEEN 1 AND 128),
  content bytea NOT NULL CHECK (octet_length(content) BETWEEN 1 AND 67108864),
  size_bytes bigint NOT NULL CHECK (size_bytes = octet_length(content)),
  sha256 text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$')
);

CREATE TABLE record_entries (
  id text PRIMARY KEY CHECK (id ~ '^v[a-z0-9]{7}$'),
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  purpose text NOT NULL CHECK (purpose IN ('full_record','training_contribution')),
  item_kind text NOT NULL CHECK (item_kind IN ('turn','sample')),
  consent_version bigint NOT NULL CHECK (consent_version >= 1),
  captured_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  expires_at timestamptz NOT NULL,
  payload_bytes bigint NOT NULL CHECK (payload_bytes >= 0),
  attachment_bytes bigint NOT NULL CHECK (attachment_bytes >= 0),
  attachment_count integer NOT NULL CHECK (attachment_count >= 0),
  state text NOT NULL DEFAULT 'active' CHECK (state IN ('active','deleting','deleted')),
  UNIQUE (id, owner_id),
  CHECK (expires_at > captured_at)
);

CREATE INDEX record_entries_owner_page_idx ON record_entries (owner_id, created_at DESC, id DESC)
  WHERE state = 'active';
CREATE INDEX record_entries_expiry_idx ON record_entries (expires_at, id)
  WHERE state = 'active';

CREATE TABLE record_turn_bodies (
  record_id text PRIMARY KEY REFERENCES record_entries(id),
  body bytea NOT NULL
);

CREATE TABLE record_sample_bodies (
  record_id text PRIMARY KEY REFERENCES record_entries(id),
  body bytea NOT NULL
);

CREATE TABLE record_attachments (
  id text PRIMARY KEY CHECK (id ~ '^g[a-z0-9]{7}$'),
  record_id text NOT NULL,
  owner_id text NOT NULL,
  file_name text NOT NULL CHECK (length(file_name) BETWEEN 1 AND 255),
  media_type text NOT NULL CHECK (length(media_type) BETWEEN 1 AND 128),
  content bytea NOT NULL CHECK (octet_length(content) BETWEEN 1 AND 67108864),
  size_bytes bigint NOT NULL CHECK (size_bytes = octet_length(content)),
  sha256 text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  FOREIGN KEY (record_id, owner_id) REFERENCES record_entries(id, owner_id)
);

CREATE INDEX record_attachments_owner_record_idx ON record_attachments (owner_id, record_id);

CREATE TABLE record_delete_tombstones (
  id text PRIMARY KEY CHECK (id ~ '^l[a-z0-9]{7}$'),
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  record_id text NOT NULL UNIQUE,
  requested_by text NOT NULL,
  state text NOT NULL CHECK (state IN ('pending','failed','completed')),
  attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  failure_code text NOT NULL DEFAULT '',
  requested_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  completed_at timestamptz,
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  FOREIGN KEY (record_id, owner_id) REFERENCES record_entries(id, owner_id),
  CHECK ((state = 'completed') = (completed_at IS NOT NULL))
);

CREATE INDEX record_delete_tombstones_pending_idx ON record_delete_tombstones (requested_at, id)
  WHERE state IN ('pending','failed');

-- +goose Down
DROP TABLE record_delete_tombstones;
DROP TABLE record_attachments;
DROP TABLE record_sample_bodies;
DROP TABLE record_turn_bodies;
DROP TABLE record_entries;
DROP TABLE record_capture_queue_attachments;
DROP TABLE record_capture_queue;
DROP TABLE record_admin_grants;
DROP TABLE record_quotas;
DROP TABLE record_consents;
