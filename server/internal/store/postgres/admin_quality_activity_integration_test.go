package postgres

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/record"
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
	if len(activity.TaskEvents) != 2 || activity.TaskEvents[0].Type != "task.status" || activity.TaskEvents[0].Count != 1 ||
		activity.TaskEvents[1].Type != "task.step" || activity.TaskEvents[1].Count != 1 {
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
	if !foundInsideDay || dailyHubOperations != 1 || dailyTaskEvents != 2 || dailyAuditEvents < 1 {
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

func TestAdminQualityCountsOnlyCurrentConsentSamplesAndRiskMetadata(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	installationID := mustID(t, domain.NewClientInstallationID)
	if _, err := store.pool.Exec(ctx, `INSERT INTO client_installations (id,owner_id,display_name,platform,expires_at)
VALUES ($1,$2,'Quality target','desktop',clock_timestamp()+interval '1 day')`, installationID, owner.ID); err != nil {
		t.Fatal(err)
	}
	created, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "admin-quality-query", "synthetic fixture"))
	if err != nil {
		t.Fatalf("submit fixture task: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	from, to := now.Add(-48*time.Hour), now.Add(time.Second)
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET status='failed',updated_at=$2 WHERE id=$1`, created.Task.ID, now.Add(-time.Hour)); err != nil {
		t.Fatalf("set failed task metadata: %v", err)
	}
	operationKeys := []string{"quality-operation-0", "quality-operation-1"}
	for index, status := range []string{"failed", "unknown"} {
		var resultHash []byte
		if status == "failed" {
			resultHash = hashBytes("quality-result")
		}
		if _, err := store.pool.Exec(ctx, `INSERT INTO task_hub_operations (
task_id,owner_id,operation_key,tool_name,target_installation_id,action,task_authorization_version,target_grant_version,lease_version,
request_hash,result_hash,status,created_at,updated_at)
VALUES ($1,$2,$3,'morde_hub_files',$4,'files',1,1,1,$5,$6,$7,$8,$8)`,
			created.Task.ID, owner.ID, operationKeys[index], installationID,
			hashBytes("quality-request"), resultHash, status, now.Add(-time.Hour)); err != nil {
			t.Fatalf("insert Hub risk metadata %s: %v", status, err)
		}
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO record_consents (owner_id,purpose,enabled,version)
VALUES ($1,'training_contribution',true,3)`, owner.ID); err != nil {
		t.Fatalf("insert training consent: %v", err)
	}
	for index, value := range []struct {
		version   int64
		expiresAt time.Time
	}{
		{version: 3, expiresAt: now.Add(24 * time.Hour)},
		{version: 2, expiresAt: now.Add(24 * time.Hour)},
		{version: 3, expiresAt: now.Add(-time.Hour)},
	} {
		id, err := record.NewRecordID()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.pool.Exec(ctx, `INSERT INTO record_entries
(id,owner_id,purpose,item_kind,consent_version,captured_at,created_at,expires_at,payload_bytes,attachment_bytes,attachment_count,state)
VALUES ($1,$2,'training_contribution','sample',$3,$4,$4,$5,32,0,0,'active')`,
			id, owner.ID, value.version, now.Add(-2*time.Hour), value.expiresAt); err != nil {
			t.Fatalf("insert sample fixture %d: %v", index, err)
		}
	}

	quality, err := store.GetAdminQuality(ctx, from, to)
	if err != nil {
		t.Fatalf("query admin quality metadata: %v", err)
	}
	if quality.SampleCount != 1 || quality.FailedTasks != 1 ||
		quality.FailedHubOperations != 1 || quality.UnknownHubOperations != 1 {
		t.Fatalf("quality summary = %#v; want one current-consent sample and each risk status", quality)
	}
}
