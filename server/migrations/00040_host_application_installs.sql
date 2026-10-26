-- +goose Up
CREATE TABLE host_application_installs (
  host_id              text NOT NULL,
  owner_id             text NOT NULL,
  request_id           text NOT NULL CHECK (request_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'),
  application_id       text NOT NULL CHECK (application_id = 'wechat'),
  runtime_generation   bigint NOT NULL CHECK (runtime_generation > 0),
  runtime_instance_id  text NOT NULL CHECK (length(runtime_instance_id) BETWEEN 1 AND 128),
  control_version      bigint NOT NULL CHECK (control_version >= 0),
  status               text NOT NULL CHECK (status IN ('unknown','accepted','running','succeeded','failed')),
  operation_id         text CHECK (operation_id ~ '^app_[a-z0-9]{1,60}$'),
  operation            jsonb,
  created_at           timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at           timestamptz NOT NULL DEFAULT clock_timestamp(),
  FOREIGN KEY (host_id, owner_id) REFERENCES hosts(id, owner_id) ON DELETE CASCADE,
  PRIMARY KEY (host_id, request_id),
  CHECK ((status='unknown' AND operation_id IS NULL AND operation IS NULL)
      OR (status<>'unknown' AND operation_id IS NOT NULL AND operation IS NOT NULL))
);

CREATE UNIQUE INDEX host_application_installs_pending
  ON host_application_installs (host_id)
  WHERE status IN ('unknown','accepted','running');

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM host_application_installs) THEN
    RAISE EXCEPTION 'cannot drop durable Host application installs';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE host_application_installs;
