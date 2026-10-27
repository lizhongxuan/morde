-- name: GetServiceInstanceID :one
SELECT instance_id::text FROM service_identity WHERE singleton = true;
