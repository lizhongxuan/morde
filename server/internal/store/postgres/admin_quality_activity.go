package postgres

import (
	"context"
	"fmt"
	"sort"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/audit"
)

var adminActivityTaskEventTypes = []string{
	"task.status", "task.step", "task.operation", "task.message", "task.batch_item",
	"task.artifact", "task.waiting_user", "task.response", "task.result",
}

type adminHubOperationKey struct {
	action string
	status string
}

// GetAdminQuality reads only status, timestamp, consent version and record metadata.
func (s *Store) GetAdminQuality(ctx context.Context, from, to time.Time) (app.AdminQuality, error) {
	result := app.AdminQuality{}
	if from.IsZero() || to.IsZero() || !to.After(from) || to.Sub(from) > 90*24*time.Hour {
		return result, fmt.Errorf("invalid admin quality time range")
	}
	if err := s.pool.QueryRow(ctx, `SELECT
  count(*) FILTER (WHERE status='failed'),
  count(*) FILTER (WHERE status='unknown')
FROM task_hub_operations WHERE created_at >= $1 AND created_at < $2`, from, to).
		Scan(&result.FailedHubOperations, &result.UnknownHubOperations); err != nil {
		return app.AdminQuality{}, fmt.Errorf("summarize admin Hub risk statuses: %w", err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE status='failed' AND updated_at >= $1 AND updated_at < $2`, from, to).
		Scan(&result.FailedTasks); err != nil {
		return app.AdminQuality{}, fmt.Errorf("summarize admin failed tasks: %w", err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM record_entries r
JOIN record_consents c ON c.owner_id=r.owner_id AND c.purpose=r.purpose
WHERE r.purpose='training_contribution' AND r.item_kind='sample' AND r.state='active'
  AND r.created_at >= $1 AND r.created_at < $2 AND r.expires_at > $2
  AND c.enabled AND c.version=r.consent_version`, from, to).
		Scan(&result.SampleCount); err != nil {
		return app.AdminQuality{}, fmt.Errorf("count current-consent training samples: %w", err)
	}
	return result, nil
}

// GetAdminActivity intentionally reads only bounded aggregation dimensions and
// timestamps; payloads, account/installation identifiers, IPs and hashes are
// never part of these queries or the returned app types.
func (s *Store) GetAdminActivity(ctx context.Context, from, to time.Time) (app.AdminActivity, error) {
	result := app.AdminActivity{
		ByDay:         make([]app.AdminActivityDay, 0),
		HubOperations: make([]app.AdminHubOperationCount, 0),
		TaskEvents:    make([]app.AdminTaskEventCount, 0),
		AuditEvents:   make([]app.AdminAuditEventCount, 0),
	}
	if from.IsZero() || to.IsZero() || !to.After(from) || to.Sub(from) > 90*24*time.Hour {
		return result, fmt.Errorf("invalid admin activity time range")
	}

	byDay := make(map[string]app.AdminActivityDay)
	hubOperations := make(map[adminHubOperationKey]int64)
	rows, err := s.pool.Query(ctx, `SELECT action,status,to_char(created_at AT TIME ZONE 'UTC','YYYY-MM-DD'),count(*)
FROM task_hub_operations WHERE created_at >= $1 AND created_at < $2
GROUP BY action,status,3 ORDER BY 3,action,status`, from, to)
	if err != nil {
		return result, fmt.Errorf("summarize admin Hub operations: %w", err)
	}
	for rows.Next() {
		var action, status, date string
		var count int64
		if err := rows.Scan(&action, &status, &date, &count); err != nil {
			rows.Close()
			return result, fmt.Errorf("read admin Hub operation summary: %w", err)
		}
		if !validAdminHubAction(action) || !validAdminHubStatus(status) {
			continue
		}
		hubOperations[adminHubOperationKey{action: action, status: status}] += count
		day := byDay[date]
		day.Date = date
		day.HubOperations += count
		byDay[date] = day
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, fmt.Errorf("read admin Hub operation summaries: %w", err)
	}
	rows.Close()

	taskEvents := make(map[string]int64)
	rows, err = s.pool.Query(ctx, `SELECT event_type,to_char(created_at AT TIME ZONE 'UTC','YYYY-MM-DD'),count(*)
FROM task_events WHERE created_at >= $1 AND created_at < $2 AND event_type=ANY($3::text[])
GROUP BY event_type,2 ORDER BY 2,event_type`, from, to, adminActivityTaskEventTypes)
	if err != nil {
		return result, fmt.Errorf("summarize admin task events: %w", err)
	}
	for rows.Next() {
		var eventType, date string
		var count int64
		if err := rows.Scan(&eventType, &date, &count); err != nil {
			rows.Close()
			return result, fmt.Errorf("read admin task event summary: %w", err)
		}
		taskEvents[eventType] += count
		day := byDay[date]
		day.Date = date
		day.TaskEvents += count
		byDay[date] = day
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, fmt.Errorf("read admin task event summaries: %w", err)
	}
	rows.Close()

	auditEvents := make(map[audit.Action]int64)
	rows, err = s.pool.Query(ctx, `SELECT action,to_char(at AT TIME ZONE 'UTC','YYYY-MM-DD'),count(*)
FROM audit_logs WHERE at >= $1 AND at < $2
GROUP BY action,2 ORDER BY 2,action`, from, to)
	if err != nil {
		return result, fmt.Errorf("summarize admin audit events: %w", err)
	}
	for rows.Next() {
		var action, date string
		var count int64
		if err := rows.Scan(&action, &date, &count); err != nil {
			rows.Close()
			return result, fmt.Errorf("read admin audit event summary: %w", err)
		}
		validAction := audit.Action(action)
		if !validAction.Valid() {
			continue
		}
		auditEvents[validAction] += count
		day := byDay[date]
		day.Date = date
		day.AuditEvents += count
		byDay[date] = day
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, fmt.Errorf("read admin audit event summaries: %w", err)
	}
	rows.Close()

	for key, count := range hubOperations {
		result.HubOperations = append(result.HubOperations, app.AdminHubOperationCount{Action: key.action, Status: key.status, Count: count})
	}
	sort.Slice(result.HubOperations, func(i, j int) bool {
		if result.HubOperations[i].Action != result.HubOperations[j].Action {
			return result.HubOperations[i].Action < result.HubOperations[j].Action
		}
		return result.HubOperations[i].Status < result.HubOperations[j].Status
	})
	for eventType, count := range taskEvents {
		result.TaskEvents = append(result.TaskEvents, app.AdminTaskEventCount{Type: eventType, Count: count})
	}
	sort.Slice(result.TaskEvents, func(i, j int) bool { return result.TaskEvents[i].Type < result.TaskEvents[j].Type })
	for action, count := range auditEvents {
		result.AuditEvents = append(result.AuditEvents, app.AdminAuditEventCount{Action: string(action), Count: count})
	}
	sort.Slice(result.AuditEvents, func(i, j int) bool { return result.AuditEvents[i].Action < result.AuditEvents[j].Action })
	for _, day := range byDay {
		result.ByDay = append(result.ByDay, day)
	}
	sort.Slice(result.ByDay, func(i, j int) bool { return result.ByDay[i].Date < result.ByDay[j].Date })
	return result, nil
}

func validAdminHubAction(action string) bool {
	switch action {
	case "screenshot", "files", "file.get", "command":
		return true
	default:
		return false
	}
}

func validAdminHubStatus(status string) bool {
	switch status {
	case "prepared", "dispatched", "succeeded", "failed", "unknown":
		return true
	default:
		return false
	}
}

var _ app.AdminQualityActivityStore = (*Store)(nil)
