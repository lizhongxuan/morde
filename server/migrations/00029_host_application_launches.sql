-- +goose Up
CREATE TABLE host_application_launches (
  host_id              text NOT NULL,
  owner_id             text NOT NULL,
  launch_id            text NOT NULL CHECK (launch_id ~ '^launch_[A-Za-z0-9][A-Za-z0-9._:-]{0,120}$'),
  application_id       text NOT NULL CHECK (application_id = 'wechat'),
  runtime_generation   bigint NOT NULL CHECK (runtime_generation > 0),
  runtime_instance_id  text NOT NULL CHECK (length(runtime_instance_id) BETWEEN 1 AND 128),
  control_version      bigint NOT NULL CHECK (control_version >= 0),
  status               text NOT NULL CHECK (status IN ('starting','running','stopping','stopped','exited','failed','unknown')),
  error_code           text NOT NULL DEFAULT '' CHECK (error_code IN ('','not_installed','architecture_unsupported','cgroup_unavailable','launch_failed','stop_unconfirmed','storage_error')),
  created_at           timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at           timestamptz NOT NULL DEFAULT clock_timestamp(),
  FOREIGN KEY (host_id, owner_id) REFERENCES hosts(id, owner_id) ON DELETE CASCADE,
  PRIMARY KEY (host_id, launch_id)
);

CREATE INDEX host_application_launches_current
  ON host_application_launches (host_id, runtime_generation, created_at DESC, launch_id DESC);

-- +goose Down
DROP TABLE host_application_launches;
