package postgres

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
)

func TestAdminActivityQueriesAggregateOnlyAllowedMetadata(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	installationID := mustID(t, domain.NewClientInstallationID)
	if _, err := store.pool.Exec(ctx, `INSERT INTO client_installations (id,owner_id,display_name,platform,expires_at)
VALUES ($1,$2,'Activity target','desktop',clock_timestamp()+interval '1 day')`, installationID, owner.ID); err != nil {
		t.Fatal(err)
	}
	submission := newTestTaskSubmission(t, owner.ID, "", "admin-activity-query", "safe fixture objective")
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit fixture task: %v", err)
	}

	requestHash := hashBytes("request-hash-secret-sentinel")
	resultHash := hashBytes("result-hash-secret-sentinel")
	now := time.Now().UTC().Truncate(time.Second)
	inside := now.Add(-time.Hour)
	outside := now.Add(-49 * time.Hour)
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_hub_operations (
task_id,owner_id,operation_key,tool_name,target_installation_id,action,task_authorization_version,target_grant_version,lease_version,
request_hash,result_hash,status,created_at,updated_at)
VALUES ($1,$2,'admin-activity-operation','morde_hub_files_sensitive-target-name',$3,'files',1,1,1,$4,$5,'succeeded',$6,$6)`,
		created.Task.ID, owner.ID, installationID, requestHash, resultHash, inside); err != nil {
		t.Fatalf("insert Hub operation fixture: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_events (task_id,sequence,event_type,payload,created_at)
VALUES ($1,91,'task.step',$2::jsonb,$3),($1,92,'task.private',$4::jsonb,$3),($1,93,'task.step',$2::jsonb,$5)`,
		created.Task.ID,
		`{"detail":{"text":"payload-secret-sentinel","user_id":"user-id-secret-sentinel"}}`, inside,
		`{"detail":{"text":"unlisted-payload-secret-sentinel"}}`, outside); err != nil {
		t.Fatalf("insert task event fixtures: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO audit_logs (actor_id,actor_kind,action,target,detail,ip,at)
VALUES ('audit-actor-id-secret-sentinel','system','auth.login','audit-target-secret-sentinel',$1::jsonb,'203.0.113.88',$2),
       ('audit-actor-id-secret-sentinel','system','private.secret','audit-target-secret-sentinel',$3::jsonb,'203.0.113.89',$2),
       ('audit-actor-id-secret-sentinel','system','auth.login','audit-target-secret-sentinel',$1::jsonb,'203.0.113.90',$4)`,
		`{"user_agent":"user-agent-secret-sentinel","detail":"audit-detail-secret-sentinel"}`, inside,
		`{"detail":"unlisted-audit-detail-secret-sentinel"}`, outside); err != nil {
		t.Fatalf("insert audit fixtures: %v", err)
	}

	from, to := now.Add(-48*time.Hour), now.Add(time.Second)
	activity, err := store.GetAdminActivity(ctx, from, to)
	if err != nil {
		t.Fatalf("query aggregate activity: %v", err)
	}
	if len(activity.HubOperations) != 1 || activity.HubOperations[0].Action != "files" ||
		activity.HubOperations[0].Status != "succeeded" || activity.HubOperations[0].Count != 1 {
		t.Fatalf("Hub operation summary = %#v", activity.HubOperations)
	}
	if len(activity.TaskEvents) != 1 || activity.TaskEvents[0].Type != "task.step" || activity.TaskEvents[0].Count != 1 {
		t.Fatalf("task event summary = %#v", activity.TaskEvents)
	}
	var loginCount int64
	for _, summary := range activity.AuditEvents {
		if summary.Action == "auth.login" {
			loginCount = summary.Count
		}
		if summary.Action == "private.secret" {
			t.Fatalf("unregistered audit action was included: %#v", summary)
		}
	}
	if loginCount != 1 {
		t.Fatalf("valid audit action count = %d; summaries=%#v", loginCount, activity.AuditEvents)
	}
	var foundInsideDay bool
	var dailyHubOperations, dailyTaskEvents, dailyAuditEvents int64
	for _, day := range activity.ByDay {
		if day.Date == inside.Format("2006-01-02") {
			foundInsideDay = true
			dailyHubOperations, dailyTaskEvents, dailyAuditEvents = day.HubOperations, day.TaskEvents, day.AuditEvents
			break
		}
	}
	if !foundInsideDay || dailyHubOperations != 1 || dailyTaskEvents != 1 || dailyAuditEvents < 1 {
		t.Fatalf("daily aggregate containing test events = %#v", activity.ByDay)
	}

	encoded, err := json.Marshal(activity)
	if err != nil {
		t.Fatal(err)
	}
	serialized := string(encoded)
	secretValues := []string{
		owner.ID, created.Task.ID, installationID, hex.EncodeToString(requestHash), hex.EncodeToString(resultHash),
		"request-hash-secret-sentinel", "result-hash-secret-sentinel", "payload-secret-sentinel", "user-id-secret-sentinel",
		"audit-actor-id-secret-sentinel", "audit-target-secret-sentinel", "audit-detail-secret-sentinel",
		"user-agent-secret-sentinel", "203.0.113.88", "203.0.113.89", "203.0.113.90", "task.private", "private.secret",
	}
	for _, secret := range secretValues {
		if strings.Contains(serialized, secret) {
			t.Errorf("aggregate response leaked %q: %s", secret, serialized)
		}
	}

	empty, err := store.GetAdminActivity(ctx, now.Add(72*time.Hour), now.Add(73*time.Hour))
	if err != nil {
		t.Fatalf("query empty activity window: %v", err)
	}
	if len(empty.ByDay) != 0 || len(empty.HubOperations) != 0 || len(empty.TaskEvents) != 0 || len(empty.AuditEvents) != 0 {
		t.Fatalf("empty activity result must use empty collections: %#v", empty)
	}
}
