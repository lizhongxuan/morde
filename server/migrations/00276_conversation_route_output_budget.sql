-- +goose Up
ALTER TABLE conversation_route_model_calls
  ADD COLUMN budget_stage text,
  ADD COLUMN budget_strategy_version text,
  ADD COLUMN budget_capability_version text,
  ADD COLUMN model_max_output_tokens bigint,
  ADD COLUMN context_window_tokens bigint,
  ADD COLUMN context_input_upper_bound_tokens bigint,
  ADD COLUMN context_safety_margin_tokens bigint,
  ADD COLUMN context_safe_tokens bigint,
  ADD COLUMN account_safe_tokens bigint,
  ADD COLUMN project_safe_tokens bigint,
  ADD COLUMN goal_tokens bigint,
  ADD COLUMN minimum_tokens bigint,
  ADD COLUMN safe_tokens bigint,
  ADD COLUMN send_tokens bigint,
  ADD COLUMN input_reserved_tokens bigint,
  ADD CONSTRAINT conversation_route_model_calls_output_budget_all_or_none_check CHECK (
    (budget_stage IS NULL AND budget_strategy_version IS NULL AND budget_capability_version IS NULL AND
     model_max_output_tokens IS NULL AND context_window_tokens IS NULL AND
     context_input_upper_bound_tokens IS NULL AND context_safety_margin_tokens IS NULL AND
     context_safe_tokens IS NULL AND account_safe_tokens IS NULL AND project_safe_tokens IS NULL AND
     goal_tokens IS NULL AND minimum_tokens IS NULL AND safe_tokens IS NULL AND send_tokens IS NULL AND
     input_reserved_tokens IS NULL)
    OR
    (budget_stage IS NOT NULL AND budget_stage='long_form' AND
     budget_strategy_version IS NOT NULL AND budget_capability_version IS NOT NULL AND
     model_max_output_tokens IS NOT NULL AND context_window_tokens IS NOT NULL AND
     context_input_upper_bound_tokens IS NOT NULL AND context_safety_margin_tokens IS NOT NULL AND
     context_safe_tokens IS NOT NULL AND account_safe_tokens IS NOT NULL AND project_safe_tokens IS NOT NULL AND
     goal_tokens IS NOT NULL AND minimum_tokens IS NOT NULL AND safe_tokens IS NOT NULL AND
     send_tokens IS NOT NULL AND input_reserved_tokens IS NOT NULL AND
     length(btrim(budget_strategy_version)) BETWEEN 1 AND 128 AND
     length(btrim(budget_capability_version)) BETWEEN 1 AND 128 AND
     model_max_output_tokens BETWEEN 1 AND 2000000 AND context_window_tokens BETWEEN 1 AND 2000000 AND
     context_input_upper_bound_tokens > 0 AND context_safety_margin_tokens > 0 AND
     context_safe_tokens BETWEEN 1 AND 2000000 AND account_safe_tokens BETWEEN 1 AND 2000000 AND
     project_safe_tokens BETWEEN 1 AND 2000000 AND
     goal_tokens BETWEEN 1 AND 2000000 AND minimum_tokens BETWEEN 1 AND 2000000 AND
     safe_tokens > 0 AND send_tokens > 0 AND input_reserved_tokens > 0 AND
     context_input_upper_bound_tokens < context_window_tokens AND
     context_safety_margin_tokens <= context_window_tokens-context_input_upper_bound_tokens AND
     context_safe_tokens=LEAST(model_max_output_tokens,
       context_window_tokens-context_input_upper_bound_tokens-context_safety_margin_tokens) AND
     safe_tokens=LEAST(context_safe_tokens,account_safe_tokens,project_safe_tokens) AND
     minimum_tokens <= send_tokens AND send_tokens=LEAST(goal_tokens,safe_tokens))
  );

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM conversation_route_model_calls WHERE budget_stage IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot discard persisted conversation route output budget plans';
  END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE conversation_route_model_calls
  DROP CONSTRAINT conversation_route_model_calls_output_budget_all_or_none_check,
  DROP COLUMN budget_stage,
  DROP COLUMN budget_strategy_version,
  DROP COLUMN budget_capability_version,
  DROP COLUMN model_max_output_tokens,
  DROP COLUMN context_window_tokens,
  DROP COLUMN context_input_upper_bound_tokens,
  DROP COLUMN context_safety_margin_tokens,
  DROP COLUMN context_safe_tokens,
  DROP COLUMN account_safe_tokens,
  DROP COLUMN project_safe_tokens,
  DROP COLUMN goal_tokens,
  DROP COLUMN minimum_tokens,
  DROP COLUMN safe_tokens,
  DROP COLUMN send_tokens,
  DROP COLUMN input_reserved_tokens;
