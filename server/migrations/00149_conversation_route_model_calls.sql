-- +goose Up
-- Routing inference is a separately attributed model call, not a synthetic Task.
ALTER TABLE model_call_ledger DROP CONSTRAINT model_call_ledger_source_check;
ALTER TABLE model_call_ledger ADD CONSTRAINT model_call_ledger_source_check
  CHECK (source IN ('client', 'chat', 'task', 'probe', 'route'));
ALTER TABLE project_budget_ledger DROP CONSTRAINT project_budget_ledger_source_check;
ALTER TABLE project_budget_ledger ADD CONSTRAINT project_budget_ledger_source_check
  CHECK (source IN ('client', 'chat', 'task', 'route'));

CREATE TABLE conversation_route_model_calls (
  message_id text PRIMARY KEY REFERENCES conversation_message_routes(message_id) ON DELETE RESTRICT,
  service_instance_id uuid NOT NULL REFERENCES service_identity(instance_id) ON DELETE RESTRICT,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  actor_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  conversation_id text NOT NULL,
  project_id text REFERENCES projects(id) ON DELETE RESTRICT,
  source text NOT NULL DEFAULT 'route' CHECK (source = 'route'),
  call_id text NOT NULL CHECK (length(call_id) BETWEEN 8 AND 128),
  request_digest bytea NOT NULL CHECK (octet_length(request_digest) = 32),
  model_id text NOT NULL REFERENCES model_connections(id) ON DELETE RESTRICT,
  model_version timestamptz NOT NULL,
  state text NOT NULL CHECK (state IN ('reserved', 'dispatched', 'settled', 'unknown')),
  dispatch_lease_owner text NOT NULL DEFAULT '',
  dispatch_lease_version bigint NOT NULL DEFAULT 0 CHECK (dispatch_lease_version >= 0),
  decision jsonb CHECK (decision IS NULL OR jsonb_typeof(decision) = 'object'),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  FOREIGN KEY (actor_id, source, call_id) REFERENCES model_call_ledger(owner_id, source, call_id) ON DELETE RESTRICT,
  FOREIGN KEY (message_id, owner_id, conversation_id, actor_id)
    REFERENCES conversation_messages(id, owner_id, conversation_id, actor_id) ON DELETE RESTRICT,
  CHECK ((state = 'settled') = (decision IS NOT NULL)),
  CHECK ((state = 'reserved' AND dispatch_lease_owner = '' AND dispatch_lease_version = 0)
    OR (state <> 'reserved' AND dispatch_lease_owner <> '' AND dispatch_lease_version > 0))
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM conversation_route_model_calls) OR EXISTS (SELECT 1 FROM model_call_ledger WHERE source='route')
     OR EXISTS (SELECT 1 FROM project_budget_ledger WHERE source='route') THEN
    RAISE EXCEPTION 'cannot discard route model call records';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE conversation_route_model_calls;
ALTER TABLE project_budget_ledger DROP CONSTRAINT project_budget_ledger_source_check;
ALTER TABLE project_budget_ledger ADD CONSTRAINT project_budget_ledger_source_check CHECK (source IN ('client', 'chat', 'task'));
ALTER TABLE model_call_ledger DROP CONSTRAINT model_call_ledger_source_check;
ALTER TABLE model_call_ledger ADD CONSTRAINT model_call_ledger_source_check CHECK (source IN ('client', 'chat', 'task', 'probe'));
