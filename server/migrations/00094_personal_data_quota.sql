-- +goose Up
CREATE TABLE personal_data_quotas (
  service_instance_id uuid NOT NULL REFERENCES service_identity(instance_id) ON DELETE RESTRICT,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  used_bytes bigint NOT NULL DEFAULT 0 CHECK (used_bytes >= 0),
  reserved_bytes bigint NOT NULL DEFAULT 0 CHECK (reserved_bytes >= 0),
  quota_bytes bigint NOT NULL DEFAULT 5368709120 CHECK (quota_bytes > 0),
  max_attachment_bytes bigint NOT NULL DEFAULT 314572800 CHECK (max_attachment_bytes > 0),
  version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (service_instance_id, owner_id)
);

CREATE TABLE personal_data_quota_reservations (
  service_instance_id uuid NOT NULL REFERENCES service_identity(instance_id) ON DELETE RESTRICT,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  reservation_id text NOT NULL CHECK (reservation_id ~ '^pq_[a-f0-9]{32}$'),
  upload_id text NOT NULL CHECK (upload_id ~ '^pu_[a-f0-9]{32}$'),
  size_bytes bigint NOT NULL CHECK (size_bytes >= 0),
  state text NOT NULL CHECK (state IN ('reserved','committed','released')),
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (service_instance_id, owner_id, reservation_id),
  UNIQUE (service_instance_id, owner_id, upload_id),
  FOREIGN KEY (service_instance_id, owner_id)
    REFERENCES personal_data_quotas(service_instance_id, owner_id) ON DELETE CASCADE
);

CREATE INDEX personal_data_quota_reservations_expiry
  ON personal_data_quota_reservations(service_instance_id, expires_at, owner_id, upload_id)
  WHERE state = 'reserved';

WITH usage_items AS (
  SELECT service_instance_id, owner_id, octet_length(body::text)::bigint AS used_bytes
  FROM personal_objects
  WHERE body IS NOT NULL

  UNION ALL

  SELECT instance.instance_id, s.owner_id,
    octet_length(jsonb_build_object(
      'objective', s.objective,
      'model', s.model_id,
      'host', s.host_id,
      'timing', s.timing,
      'budgets', jsonb_build_object(
        'active_seconds', s.budget_active_seconds,
        'tool_calls', s.budget_tool_calls,
        'model_tokens', s.budget_model_tokens
      ),
      'misfire', s.misfire_policy,
      'notification', to_jsonb(s.notification_events),
      'observation', s.observation_spec
    )::text)::bigint
  FROM schedules AS s
  CROSS JOIN service_identity AS instance
  WHERE instance.singleton = true

  UNION ALL

  SELECT service_instance_id, owner_id, size_bytes
  FROM personal_attachment_uploads AS attachment
  WHERE state = 'committed'
    AND EXISTS (
      SELECT 1 FROM personal_objects AS object
      WHERE object.service_instance_id = attachment.service_instance_id
        AND object.owner_id = attachment.owner_id
        AND object.object_type = 'library_item'
        AND object.body IS NOT NULL
        AND object.body->>'attachment_id' = attachment.attachment_id
    )
), owner_usage AS (
  SELECT service_instance_id, owner_id, SUM(used_bytes)::bigint AS used_bytes
  FROM usage_items
  GROUP BY service_instance_id, owner_id
)
INSERT INTO personal_data_quotas (
  service_instance_id, owner_id, used_bytes, reserved_bytes, quota_bytes,
  max_attachment_bytes, version
)
SELECT instance.instance_id, u.id, COALESCE(usage.used_bytes, 0), 0,
  5368709120, 314572800, 1
FROM service_identity AS instance
CROSS JOIN users AS u
LEFT JOIN owner_usage AS usage
  ON usage.service_instance_id = instance.instance_id AND usage.owner_id = u.id
WHERE instance.singleton = true
ON CONFLICT (service_instance_id, owner_id) DO NOTHING;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM personal_data_quota_reservations WHERE state = 'reserved')
     OR EXISTS (SELECT 1 FROM personal_data_quotas
       WHERE used_bytes <> 0 OR reserved_bytes <> 0 OR quota_bytes <> 5368709120
         OR max_attachment_bytes <> 314572800 OR version <> 1) THEN
    RAISE EXCEPTION 'cannot roll back personal data quotas while quota state or personal usage exists';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE personal_data_quota_reservations;
DROP TABLE personal_data_quotas;
