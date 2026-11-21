-- +goose Up
ALTER TABLE task_step_details
  ADD COLUMN content_evidence_sha256 bytea
    CHECK (content_evidence_sha256 IS NULL OR octet_length(content_evidence_sha256) = 32);

-- +goose Down
ALTER TABLE task_step_details DROP COLUMN content_evidence_sha256;
