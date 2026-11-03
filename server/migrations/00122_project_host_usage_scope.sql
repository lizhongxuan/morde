-- +goose Up
CREATE INDEX host_usage_samples_project_host
  ON host_usage_samples(project_id, host_id, id)
  WHERE attribution_state = 'project';
CREATE INDEX host_usage_intervals_project_host_time
  ON host_usage_intervals(project_id, host_id, ended_at, id)
  WHERE attribution_state = 'project';

-- +goose Down
DROP INDEX host_usage_intervals_project_host_time;
DROP INDEX host_usage_samples_project_host;
