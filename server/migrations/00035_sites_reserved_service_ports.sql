-- +goose Up
ALTER TABLE site_releases
  ADD CONSTRAINT site_releases_service_port_policy_check
  CHECK (service_port BETWEEN 1024 AND 65535 AND service_port NOT IN (8080, 5900, 6080, 8090, 8091, 8100, 9222));

-- +goose Down
ALTER TABLE site_releases DROP CONSTRAINT site_releases_service_port_policy_check;
