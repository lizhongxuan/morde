-- +goose Up
-- E05: approval card body fields on task_pending_requests. All columns are
-- nullable: legacy rows have no evidence fields, and costs are unknown until
-- a trusted source exists (never a fabricated 0).
ALTER TABLE task_pending_requests
  ADD COLUMN evidence_pointer text,
  ADD COLUMN this_cost_micros bigint,
  ADD COLUMN total_cost_micros bigint,
  ADD COLUMN trigger_rule text,
  ADD COLUMN rejection_consequence text;

ALTER TABLE task_pending_requests
  ADD CONSTRAINT task_pending_requests_evidence_pointer_check CHECK (
    evidence_pointer IS NULL OR evidence_pointer LIKE 'step:%'
  ),
  ADD CONSTRAINT task_pending_requests_this_cost_micros_check CHECK (
    this_cost_micros IS NULL OR this_cost_micros >= 0
  ),
  ADD CONSTRAINT task_pending_requests_total_cost_micros_check CHECK (
    total_cost_micros IS NULL OR total_cost_micros >= 0
  ),
  ADD CONSTRAINT task_pending_requests_trigger_rule_check CHECK (
    trigger_rule IS NULL OR trigger_rule IN (
      'host_sensitive_tool', 'external_mcp_tool', 'workspace_live_write', 'media_publish'
    )
  );

-- +goose Down
ALTER TABLE task_pending_requests
  DROP CONSTRAINT task_pending_requests_trigger_rule_check,
  DROP CONSTRAINT task_pending_requests_total_cost_micros_check,
  DROP CONSTRAINT task_pending_requests_this_cost_micros_check,
  DROP CONSTRAINT task_pending_requests_evidence_pointer_check,
  DROP COLUMN rejection_consequence,
  DROP COLUMN trigger_rule,
  DROP COLUMN total_cost_micros,
  DROP COLUMN this_cost_micros,
  DROP COLUMN evidence_pointer;
