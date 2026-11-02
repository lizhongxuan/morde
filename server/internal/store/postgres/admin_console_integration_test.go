package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestAdminConsoleQueriesAggregateAndPaginateSafely(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 20_000, 40_000)
	now := time.Now().UTC().Truncate(time.Microsecond)
	olderInstall := "i1234567"
	newerInstall := "i7654321"
	if _, err := pool.Exec(ctx, `INSERT INTO client_installations(id,owner_id,display_name,platform,expires_at,created_at,last_used_at)
VALUES ($1,$2,'Older','ios',$3,$4,$5),($6,$2,'Newer','ios',$7,$8,$9),('i2345678',$2,'Desktop','desktop',NULL,$10,NULL)`,
		olderInstall, owner.ID, now.Add(24*time.Hour), now.Add(-48*time.Hour), now.Add(-48*time.Hour),
		newerInstall, now.Add(24*time.Hour), now.Add(-6*time.Hour), now.Add(-24*time.Hour), now.Add(-12*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for index, state := range []string{"pending", "claimed"} {
		id := []string{"t1234567", "t7654321"}[index]
		if _, err := pool.Exec(ctx, `INSERT INTO client_authorizations(id,display_name,platform,wants_workspace_sync,secret_hash,user_code_hash,state,owner_id,expires_at)
VALUES ($1,$2,'desktop',false,$3,$4,$5,$6,$7)`, id, "Authorization", make([]byte, 32), append([]byte{byte(index + 1)}, make([]byte, 31)...), state, owner.ID, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	settledAt := now.Add(-24 * time.Hour)
	if _, err := pool.Exec(ctx, `INSERT INTO model_call_ledger(owner_id,source,call_id,request_digest,model_id,provider_model,reserved_tokens,state,input_tokens,output_tokens,created_at,settled_at)
VALUES ($1,'client','call-0001',$2,'p1234567','model-a',20,'settled',8,3,$3,$3),
($1,'chat','call-0002',$4,'p1234567','model-a',10,'unknown',NULL,NULL,$5,$5)`, owner.ID, make([]byte, 32), settledAt, append([]byte{1}, make([]byte, 31)...), now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	serviceInstanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO account_agent_runtimes(service_instance_id,owner_id,runtime_id,runtime_generation,state,lease_owner,lease_version,execution_epoch,lease_expires_at,max_concurrent_tasks)
VALUES ($1,$2,'runtime-running',1,'running','lease',1,1,$3,1),($1,$4,'runtime-failed',1,'failed','lease',1,1,NULL,1)`,
		serviceInstanceID, owner.ID, now.Add(time.Hour), createTestUser(t, store, 3, 20_000, 40_000).ID); err != nil {
		t.Fatal(err)
	}

	users, totalUsers, err := store.ListAdminUsersPage(ctx, nil, 1, 0)
	if err != nil || totalUsers != 2 || len(users) != 1 || users[0].PasswordHash != "" {
		t.Fatalf("admin user page = %#v total=%d err=%v", users, totalUsers, err)
	}
	installations, err := store.ListAdminInstallations(ctx, owner.ID, 1, 0)
	if err != nil || installations.Total != 3 || len(installations.Items) != 1 || installations.Items[0].ID != newerInstall {
		t.Fatalf("admin installation page = %#v err=%v", installations, err)
	}
	authorizations, err := store.ListAdminAuthorizations(ctx, owner.ID, 1, 0)
	if err != nil || authorizations.Total != 2 || len(authorizations.Items) != 1 || authorizations.Items[0].ID != "t7654321" {
		t.Fatalf("admin authorization page = %#v err=%v", authorizations, err)
	}
	calls, err := store.ListAdminModelCalls(ctx, owner.ID, 1, 0)
	if err != nil || calls.Total != 2 || len(calls.Items) != 1 || calls.Items[0].CallID != "call-0001" ||
		calls.Items[0].InputTokens == nil || *calls.Items[0].InputTokens != 8 || calls.Items[0].OutputTokens == nil || *calls.Items[0].OutputTokens != 3 {
		t.Fatalf("admin model call page = %#v err=%v", calls, err)
	}
	agentStates, err := store.ListAdminAgentStates(ctx)
	if err != nil || agentStates["running"] != 1 || agentStates["failed"] != 1 {
		t.Fatalf("admin agent states = %#v err=%v", agentStates, err)
	}
	usage, err := store.GetAdminUsage(ctx, now.Add(-7*24*time.Hour), now)
	if err != nil || usage.Summary.Calls != 2 || usage.Summary.SettledCalls != 1 || usage.Summary.UnknownCalls != 1 ||
		usage.Summary.InputTokens != 8 || usage.Summary.OutputTokens != 3 || usage.Summary.ActiveAccounts != 1 || len(usage.ByDay) != 2 || len(usage.ByModel) != 1 {
		t.Fatalf("admin usage = %#v err=%v", usage, err)
	}
	stats, err := store.GetAdminStats(ctx, now)
	weekYear, week := now.ISOWeek()
	var iosStats struct{ Installations, ActiveInstallations, Users int64 }
	for _, platform := range stats.Platforms {
		if platform.Platform == "ios" {
			iosStats.Installations, iosStats.ActiveInstallations, iosStats.Users = platform.Installations, platform.ActiveInstallations, platform.Users
		}
	}
	if err != nil || stats.Users.Total != 2 || stats.Users.Approved != 2 || len(stats.Platforms) != 2 || len(stats.Cohorts) != 1 ||
		stats.Cohorts[0].Week != fmt.Sprintf("%04d-W%02d", weekYear, week) || stats.Cohorts[0].Signups != 2 || stats.Cohorts[0].WithInstallation != 1 ||
		iosStats.Installations != 2 || iosStats.ActiveInstallations != 2 || iosStats.Users != 1 {
		t.Fatalf("admin stats = %#v err=%v", stats, err)
	}
}
