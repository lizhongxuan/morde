package postgres

import (
	"context"
	"fmt"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

func (s *Store) ListRecoverableCloudDSHCaptures(ctx context.Context, limit int32) ([]app.CloudDSHCaptureCandidate, error) {
	if limit < 1 || limit > 100 {
		return nil, domain.InvalidArgument("record_capture_recovery", "record capture recovery limit is invalid")
	}
	rows, err := s.pool.Query(ctx, `SELECT t.id,t.owner_id,t.auth_kind,t.auth_token_id,
COALESCE(t.auth_client_installation_id,''),t.objective,t.result,c.version
FROM tasks t
JOIN task_events e ON e.task_id=t.id AND e.sequence=t.event_seq AND e.event_type='task.result'
JOIN record_consents c ON c.owner_id=t.owner_id AND c.purpose='full_record'
JOIN users u ON u.id=t.owner_id AND u.status='approved'
JOIN tokens tok ON tok.id=t.auth_token_id AND tok.owner_id=t.owner_id AND tok.kind=t.auth_kind
  AND tok.revoked_at IS NULL AND tok.expires_at>clock_timestamp()
  AND COALESCE(tok.client_installation_id,'')=COALESCE(t.auth_client_installation_id,'')
LEFT JOIN client_installations ci ON ci.id=t.auth_client_installation_id AND ci.owner_id=t.owner_id
  AND ci.revoked_at IS NULL AND (ci.expires_at IS NULL OR ci.expires_at>clock_timestamp())
WHERE t.status='completed' AND t.execution_location='server' AND t.auth_kind IN ('session','client')
  AND e.payload->'detail'->>'source'='cloud_dsh'
  AND c.enabled AND c.updated_at<t.finished_at
  AND (t.auth_kind<>'client' OR ci.id IS NOT NULL)
  AND NOT EXISTS (
    SELECT 1 FROM record_capture_queue q
    WHERE q.owner_id=t.owner_id AND q.source_task_id=t.id AND q.purpose='full_record'
  )
ORDER BY t.finished_at,t.id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list recoverable cloud dsh record captures: %w", err)
	}
	defer rows.Close()
	candidates := make([]app.CloudDSHCaptureCandidate, 0, limit)
	for rows.Next() {
		var candidate app.CloudDSHCaptureCandidate
		var kind string
		if err := rows.Scan(&candidate.TaskID, &candidate.Principal.UserID, &kind, &candidate.Principal.TokenID,
			&candidate.Principal.ClientInstallationID, &candidate.Objective, &candidate.Result, &candidate.ConsentVersion); err != nil {
			return nil, fmt.Errorf("scan recoverable cloud dsh record capture: %w", err)
		}
		candidate.Principal.Kind = identity.Kind(kind)
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read recoverable cloud dsh record captures: %w", err)
	}
	return candidates, nil
}
