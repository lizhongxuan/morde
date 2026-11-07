-- +goose Up
-- A missing predecessor may be checked for independence once, and later the
-- complete causal context may be classified once. Both calls retain their
-- own budget ledger and unknown dispatch evidence.
ALTER TABLE conversation_route_model_calls
  ADD COLUMN phase text NOT NULL DEFAULT 'normal' CHECK (phase IN ('normal', 'missing_predecessor'));
ALTER TABLE conversation_route_model_calls DROP CONSTRAINT conversation_route_model_calls_pkey;
ALTER TABLE conversation_route_model_calls ADD PRIMARY KEY (message_id, phase);
CREATE UNIQUE INDEX conversation_route_model_calls_one_unresolved
  ON conversation_route_model_calls(message_id) WHERE state IN ('reserved', 'dispatched', 'unknown');

ALTER TABLE conversation_message_routes
  ADD COLUMN awaiting_predecessor boolean NOT NULL DEFAULT false;
CREATE INDEX conversation_message_routes_awaiting_predecessor
  ON conversation_message_routes(service_instance_id, actor_id, conversation_id, previous_client_message_id)
  WHERE awaiting_predecessor AND state='waiting_user';

-- +goose Down
-- The second phase is billable history and cannot be silently discarded.
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM conversation_route_model_calls WHERE phase <> 'normal')
     OR EXISTS (SELECT 1 FROM conversation_message_routes WHERE awaiting_predecessor) THEN
    RAISE EXCEPTION 'cannot remove causal routing while evidence or waiting messages exist';
  END IF;
END $$;
-- +goose StatementEnd
DROP INDEX conversation_message_routes_awaiting_predecessor;
ALTER TABLE conversation_message_routes DROP COLUMN awaiting_predecessor;
DROP INDEX conversation_route_model_calls_one_unresolved;
ALTER TABLE conversation_route_model_calls DROP CONSTRAINT conversation_route_model_calls_pkey;
ALTER TABLE conversation_route_model_calls ADD PRIMARY KEY (message_id);
ALTER TABLE conversation_route_model_calls DROP COLUMN phase;
