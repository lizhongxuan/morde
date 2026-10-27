-- +goose Up
CREATE TABLE IF NOT EXISTS service_identity (
  singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  instance_id uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE
);

INSERT INTO service_identity (singleton) VALUES (true)
ON CONFLICT (singleton) DO NOTHING;

-- +goose Down
-- Keep the instance identity when rolling back, so a later upgrade preserves
-- the account namespace already used by clients.
SELECT 1;
