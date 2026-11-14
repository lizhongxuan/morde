-- +goose Up
-- A bounded, non-sliding collection window for newly accepted conversation
-- routes. Retry backoff continues to use available_after independently.
ALTER TABLE conversation_message_routes
  ADD COLUMN aggregation_started_at timestamptz,
  ADD COLUMN aggregation_deadline timestamptz,
  ADD COLUMN aggregation_state text;

-- Existing routes are never delayed by this feature after deployment.
UPDATE conversation_message_routes
SET aggregation_started_at = created_at,
    aggregation_deadline = created_at,
    aggregation_state = 'settled';

ALTER TABLE conversation_message_routes
  ALTER COLUMN aggregation_started_at SET NOT NULL,
  ALTER COLUMN aggregation_started_at SET DEFAULT statement_timestamp(),
  ALTER COLUMN aggregation_deadline SET NOT NULL,
  ALTER COLUMN aggregation_deadline SET DEFAULT statement_timestamp() + interval '350 milliseconds',
  ALTER COLUMN aggregation_state SET NOT NULL,
  ALTER COLUMN aggregation_state SET DEFAULT 'open',
  ADD CONSTRAINT conversation_message_routes_aggregation_state_check
    CHECK (aggregation_state IN ('open', 'waiting', 'settled', 'cancelled')),
  ADD CONSTRAINT conversation_message_routes_aggregation_window_check
    CHECK (aggregation_deadline >= aggregation_started_at);

CREATE INDEX conversation_message_routes_aggregation_claimable
  ON conversation_message_routes(aggregation_deadline, created_at, message_id)
  WHERE state IN ('pending', 'processing') AND aggregation_state = 'waiting';

CREATE INDEX conversation_message_routes_aggregation_receipts
  ON conversation_message_routes(service_instance_id, owner_id, actor_id, conversation_id, aggregation_started_at);

-- +goose Down
-- Aggregate decisions and pending-root cancellations are durable routing
-- evidence. Refuse rollback rather than making those routes executable again.
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM conversation_message_routes r WHERE r.aggregation_state IN ('waiting','cancelled')
     OR (r.aggregation_state='settled' AND r.aggregation_started_at<>r.aggregation_deadline)
     OR (r.aggregation_state='open' AND EXISTS (
       SELECT 1 FROM conversation_message_routes child WHERE child.duplicate_of_message_id=r.message_id
     ))) THEN
    RAISE EXCEPTION 'cannot discard conversation route aggregation evidence';
  END IF;
END $$;
-- +goose StatementEnd

DROP INDEX conversation_message_routes_aggregation_receipts;
DROP INDEX conversation_message_routes_aggregation_claimable;
ALTER TABLE conversation_message_routes
  DROP CONSTRAINT conversation_message_routes_aggregation_window_check,
  DROP CONSTRAINT conversation_message_routes_aggregation_state_check,
  DROP COLUMN aggregation_state,
  DROP COLUMN aggregation_deadline,
  DROP COLUMN aggregation_started_at;
