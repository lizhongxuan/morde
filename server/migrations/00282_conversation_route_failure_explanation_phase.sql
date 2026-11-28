-- +goose Up
ALTER TABLE conversation_route_model_calls
  DROP CONSTRAINT conversation_route_model_calls_phase_check,
  ADD CONSTRAINT conversation_route_model_calls_phase_check
    CHECK (phase IN ('normal', 'missing_predecessor', 'failure_explanation')
      OR phase ~ '^result_review_[1-9][0-9]*$');

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM conversation_route_model_calls WHERE phase = 'failure_explanation') THEN
    RAISE EXCEPTION 'cannot discard route failure explanation call records';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE conversation_route_model_calls
  DROP CONSTRAINT conversation_route_model_calls_phase_check,
  ADD CONSTRAINT conversation_route_model_calls_phase_check
    CHECK (phase IN ('normal', 'missing_predecessor') OR phase ~ '^result_review_[1-9][0-9]*$');
