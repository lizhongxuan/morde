-- +goose Up
ALTER TABLE tokens ADD COLUMN preview_port integer;
ALTER TABLE tokens ADD CONSTRAINT tokens_preview_port_check CHECK (
  preview_port IS NULL OR (
    kind = 'ticket' AND preview_port BETWEEN 1 AND 65535
    AND preview_port NOT IN (8080, 5900, 6080, 8091, 8100, 9222)
  )
);

-- +goose Down
ALTER TABLE tokens DROP COLUMN preview_port;
