-- name: PruneExpiredTaskWorkbenchObservationTickets :exec
WITH stale AS (
  SELECT ticket_hash FROM task_workbench_observation_tickets
  WHERE expires_at < clock_timestamp() - interval '1 day'
  ORDER BY created_at LIMIT 500
)
DELETE FROM task_workbench_observation_tickets AS tickets USING stale
WHERE tickets.ticket_hash=stale.ticket_hash;

-- name: CreateTaskWorkbenchObservationTicket :exec
INSERT INTO task_workbench_observation_tickets
(ticket_hash,nonce_hash,task_id,host_id,actor_user_id,session_token_id,runtime_generation,read_scope,expires_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9);

-- name: GetTaskWorkbenchObservationTicket :one
SELECT ticket_hash,nonce_hash,session_hash,task_id,host_id,actor_user_id,session_token_id,runtime_generation,
       read_scope,expires_at,used_at,session_expires_at,created_at
FROM task_workbench_observation_tickets
WHERE ticket_hash=$1 AND used_at IS NULL AND expires_at > clock_timestamp();

-- name: ConsumeTaskWorkbenchObservationTicket :one
UPDATE task_workbench_observation_tickets
SET session_hash=$2,used_at=clock_timestamp(),session_expires_at=$4
WHERE ticket_hash=$1 AND used_at IS NULL AND session_hash IS NULL AND expires_at > clock_timestamp()
  AND runtime_generation=$3
RETURNING ticket_hash,nonce_hash,session_hash,task_id,host_id,actor_user_id,session_token_id,runtime_generation,
          read_scope,expires_at,used_at,session_expires_at,created_at;

-- name: GetTaskWorkbenchObservationSession :one
SELECT ticket_hash,nonce_hash,session_hash,task_id,host_id,actor_user_id,session_token_id,runtime_generation,
       read_scope,expires_at,used_at,session_expires_at,created_at
FROM task_workbench_observation_tickets
WHERE session_hash=$1 AND used_at IS NOT NULL AND session_expires_at > $2;
