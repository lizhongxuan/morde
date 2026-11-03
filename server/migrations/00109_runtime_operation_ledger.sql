-- +goose Up
CREATE TABLE host_runtime_operation_ledger_heads (
  host_id text NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
  runtime_generation bigint NOT NULL CHECK (runtime_generation > 0),
  namespace text NOT NULL CHECK (namespace IN ('runtime_core', 'browser')),
  event_count bigint NOT NULL DEFAULT 0 CHECK (event_count BETWEEN 0 AND 65536),
  head_hash text NOT NULL DEFAULT '' CHECK (head_hash = '' OR head_hash ~ '^[0-9a-f]{64}$'),
  total_bytes bigint NOT NULL DEFAULT 0 CHECK (total_bytes BETWEEN 0 AND 134217728),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (host_id, runtime_generation, namespace),
  CHECK ((event_count = 0 AND head_hash = '' AND total_bytes = 0) OR (event_count > 0 AND head_hash <> ''))
);

CREATE TABLE host_runtime_operation_ledger_events (
  host_id text NOT NULL,
  runtime_generation bigint NOT NULL,
  namespace text NOT NULL,
  sequence bigint NOT NULL CHECK (sequence BETWEEN 1 AND 65536),
  kind text NOT NULL CHECK (length(kind) BETWEEN 1 AND 128),
  data jsonb NOT NULL,
  data_canonical bytea NOT NULL CHECK (octet_length(data_canonical) BETWEEN 1 AND 8388608),
  previous_hash text NOT NULL CHECK (previous_hash = '' OR previous_hash ~ '^[0-9a-f]{64}$'),
  event_hash text NOT NULL CHECK (event_hash ~ '^[0-9a-f]{64}$'),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (host_id, runtime_generation, namespace, sequence),
  FOREIGN KEY (host_id, runtime_generation, namespace)
    REFERENCES host_runtime_operation_ledger_heads(host_id, runtime_generation, namespace) ON DELETE CASCADE,
  CHECK (convert_from(data_canonical, 'UTF8')::jsonb = data)
);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM host_runtime_operation_ledger_events) THEN
    RAISE EXCEPTION 'cannot roll back Host Runtime operation ledger after events have been persisted';
  END IF;
END;
$$;
-- +goose StatementEnd

DROP TABLE host_runtime_operation_ledger_events;
DROP TABLE host_runtime_operation_ledger_heads;
