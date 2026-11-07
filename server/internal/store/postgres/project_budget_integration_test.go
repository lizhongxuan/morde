package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

func TestProjectBudgetPersistenceAndLedgerPagination(t *testing.T) {
	store, pool := newProjectBudgetTestStore(t)
	ctx := context.Background()
	owner := createProjectBudgetTestUser(t, store)
	viewer := createProjectBudgetTestUser(t, store)
	outsider := createProjectBudgetTestUser(t, store)
	projectID, err := project.NewID()
	if err != nil {
		t.Fatal(err)
	}
	value, err := project.New(projectID, owner.ID, "Budget fixture", "project budget persistence")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateProject(ctx, owner.ID, value, "project-create-01"); err != nil {
		t.Fatalf("create project fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO project_members(project_id,user_id,role,version) VALUES ($1,$2,'viewer',1)`, projectID, viewer.ID); err != nil {
		t.Fatalf("add project viewer fixture: %v", err)
	}

	if _, err := store.GetProjectBudget(ctx, outsider.ID, projectID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("nonmember budget read = %v, want not found", err)
	}
	if view, err := store.GetProjectBudget(ctx, owner.ID, projectID); err != nil || view.Configured || view.Budget != (project.Budget{}) {
		t.Fatalf("unset project budget = %#v, %v", view, err)
	}
	if _, err := store.SetProjectBudget(ctx, viewer.ID, projectID,
		project.Budget{Currency: "USD", LimitMicros: 10_000, Version: 1}, 0, "viewer-budget-01"); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("viewer budget write = %v, want forbidden", err)
	}

	firstBudget := project.Budget{Currency: "USD", LimitMicros: 10_000, Version: 1}
	first, err := store.SetProjectBudget(ctx, owner.ID, projectID, firstBudget, 0, "budget-key-001")
	if err != nil || !first.Configured || first.Budget != firstBudget {
		t.Fatalf("set first budget = %#v, %v", first, err)
	}
	replayed, err := store.SetProjectBudget(ctx, owner.ID, projectID, firstBudget, 0, "budget-key-001")
	if err != nil || replayed.Budget != firstBudget {
		t.Fatalf("replay first budget = %#v, %v", replayed, err)
	}
	changed := project.Budget{Currency: "USD", LimitMicros: 11_000, Version: 1}
	if _, err := store.SetProjectBudget(ctx, owner.ID, projectID, changed, 0, "budget-key-001"); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("changed idempotency request = %v, want idempotency conflict", err)
	}
	if _, err := store.SetProjectBudget(ctx, owner.ID, projectID,
		project.Budget{Currency: "USD", LimitMicros: 11_000, Version: 1}, 0, "budget-stale-01"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale budget version = %v, want conflict", err)
	}
	if view, err := store.GetProjectBudget(ctx, viewer.ID, projectID); err != nil || !view.Configured || view.Budget != firstBudget {
		t.Fatalf("viewer budget read = %#v, %v", view, err)
	}

	modelID := "p1234567"
	baseTime := time.Date(2026, 11, 2, 4, 5, 6, 0, time.UTC)
	ledgerDigest := make([]byte, 32)
	insertLedger := func(source, callID string, taskID any, status string, reservedInput, reservedOutput, reservedMicros int64, actualInput, actualOutput, actualMicros any, createdAt time.Time) {
		t.Helper()
		_, err := pool.Exec(ctx, `INSERT INTO project_budget_ledger
(project_id,source,call_id,actor_id,task_id,model_id,rate_provider,rate_model,rate_version,currency,
 input_micros_per_million,output_micros_per_million,reserved_input_tokens,reserved_output_tokens,reserved_micros,
 status,actual_input_tokens,actual_output_tokens,actual_micros,request_digest,created_at)
VALUES ($1,$2,$3,$4,$5,$6,'provider','model-v1',1,'USD',100,200,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			projectID, source, callID, owner.ID, taskID, modelID, reservedInput, reservedOutput, reservedMicros,
			status, actualInput, actualOutput, actualMicros, ledgerDigest, createdAt)
		if err != nil {
			t.Fatalf("insert project ledger row %q: %v", callID, err)
		}
	}
	insertLedger("client", "client-call-001", nil, "settled", 5, 0, 100, int64(10), int64(5), int64(200), baseTime.Add(-time.Second))
	insertLedger("chat", "chat-call-001", nil, "reserved", 2, 0, 300, nil, nil, nil, baseTime)

	secondBudget := project.Budget{Currency: "USD", LimitMicros: 500, Version: 2}
	updated, err := store.SetProjectBudget(ctx, owner.ID, projectID, secondBudget, 1, "budget-key-002")
	if err != nil || updated.Budget != secondBudget {
		t.Fatalf("set budget at committed usage boundary = %#v, %v", updated, err)
	}
	if _, err := store.SetProjectBudget(ctx, owner.ID, projectID,
		project.Budget{Currency: "EUR", LimitMicros: 10_000, Version: 3}, 2, "budget-currency-01"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("currency change with ledger usage = %v, want conflict", err)
	}
	if _, err := store.SetProjectBudget(ctx, owner.ID, projectID,
		project.Budget{Currency: "USD", LimitMicros: 499, Version: 3}, 2, "budget-below-usage"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("budget below settled plus reserved amount = %v, want conflict", err)
	}
	insertLedger("task", "task-call-001", "q0000000", "unknown", 0, 3, 400, nil, nil, nil, baseTime.Add(time.Second))
	if _, err := store.SetProjectBudget(ctx, owner.ID, projectID,
		project.Budget{Currency: "USD", LimitMicros: 20_000, Version: 3}, 2, "budget-unknown-01"); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("budget write while usage is unknown = %v, want invalid state", err)
	}

	view, err := store.GetProjectBudget(ctx, viewer.ID, projectID)
	if err != nil || view.Budget != secondBudget || view.SpentMicros != 200 || view.ReservedMicros != 700 || !view.HasUnknown {
		t.Fatalf("budget totals = %#v, %v; want spent=200 reserved=700 unknown=true", view, err)
	}
	firstPage, err := store.ListProjectLedgerEntries(ctx, viewer.ID, projectID, app.PageRequest{Limit: 2})
	if err != nil || len(firstPage.Items) != 2 || firstPage.NextCursor == "" {
		t.Fatalf("first ledger page = %#v, %v", firstPage, err)
	}
	if firstPage.Items[0].CallID != "task-call-001" || firstPage.Items[0].Status != app.ProjectLedgerUnknown ||
		firstPage.Items[0].TaskID != "q0000000" || firstPage.Items[0].ActualUsage != nil || firstPage.Items[0].ActualMicros != nil {
		t.Fatalf("newest ledger metadata = %#v", firstPage.Items[0])
	}
	if firstPage.Items[1].CallID != "chat-call-001" || firstPage.Items[1].Status != app.ProjectLedgerReserved || firstPage.Items[1].ReservedMicros != 300 {
		t.Fatalf("second ledger metadata = %#v", firstPage.Items[1])
	}
	secondPage, err := store.ListProjectLedgerEntries(ctx, viewer.ID, projectID, app.PageRequest{Limit: 2, Cursor: firstPage.NextCursor})
	if err != nil || len(secondPage.Items) != 1 || secondPage.NextCursor != "" || secondPage.Items[0].CallID != "client-call-001" {
		t.Fatalf("second ledger page = %#v, %v", secondPage, err)
	}
	settled := secondPage.Items[0]
	if settled.ActualUsage == nil || *settled.ActualUsage != (project.Usage{InputTokens: 10, OutputTokens: 5}) ||
		settled.ActualMicros == nil || *settled.ActualMicros != 200 || settled.RateVersion != 1 || settled.Currency != "USD" {
		t.Fatalf("settled ledger metadata = %#v", settled)
	}
	if _, err := store.ListProjectLedgerEntries(ctx, outsider.ID, projectID, app.PageRequest{Limit: 1}); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("nonmember ledger read = %v, want not found", err)
	}
	if _, err := store.ListProjectLedgerEntries(ctx, viewer.ID, projectID, app.PageRequest{Limit: 1, Cursor: firstPage.NextCursor}); err != nil {
		t.Fatalf("same-project cursor replay: %v", err)
	}
	otherProjectID, err := project.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListProjectLedgerEntries(ctx, owner.ID, otherProjectID, app.PageRequest{Limit: 1, Cursor: firstPage.NextCursor}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("cross-project ledger cursor = %v, want invalid argument", err)
	}

	var ledgerCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM project_budget_ledger WHERE project_id=$1`, projectID).Scan(&ledgerCount); err != nil {
		t.Fatal(err)
	}
	var accountLedgerCount, accountBudgetCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM model_call_ledger WHERE owner_id=$1`, owner.ID).Scan(&accountLedgerCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM model_account_budgets WHERE owner_id=$1`, owner.ID).Scan(&accountBudgetCount); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReserveProjectModelCall(ctx, app.ProjectModelReservation{
		ProjectID: projectID, ActorID: owner.ID, Source: app.ModelCallClient, CallID: "disabled-call-01",
	}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("incomplete account/project reserve = %v, want invalid argument", err)
	}
	if err := store.SettleProjectModelCall(ctx, app.SettleProjectModelCallCommand{
		ProjectID: projectID, Source: app.ModelCallClient, CallID: "disabled-call-01",
	}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("incomplete account/project settlement = %v, want invalid argument", err)
	}
	var ledgerCountAfter int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM project_budget_ledger WHERE project_id=$1`, projectID).Scan(&ledgerCountAfter); err != nil {
		t.Fatal(err)
	}
	if ledgerCountAfter != ledgerCount {
		t.Fatalf("unsupported reservation/settlement changed ledger rows: %d -> %d", ledgerCount, ledgerCountAfter)
	}
	var accountLedgerCountAfter, accountBudgetCountAfter int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM model_call_ledger WHERE owner_id=$1`, owner.ID).Scan(&accountLedgerCountAfter); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM model_account_budgets WHERE owner_id=$1`, owner.ID).Scan(&accountBudgetCountAfter); err != nil {
		t.Fatal(err)
	}
	if accountLedgerCountAfter != accountLedgerCount || accountBudgetCountAfter != accountBudgetCount {
		t.Fatalf("unsupported reservation/settlement changed account usage state: ledger %d -> %d, budget %d -> %d",
			accountLedgerCount, accountLedgerCountAfter, accountBudgetCount, accountBudgetCountAfter)
	}
}

func TestPostgresProjectModelReservationsAndSettlementsUseBothLedgers(t *testing.T) {
	store, principal, accountCall := modelUsageFixture(t)
	ctx := context.Background()
	projectOwner := createProjectBudgetTestUser(t, store)
	projectID, err := project.NewID()
	if err != nil {
		t.Fatal(err)
	}
	value, err := project.New(projectID, projectOwner.ID, "Metered project", "atomic project model budget")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateProject(ctx, projectOwner.ID, value, "project-create-01"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO project_members(project_id,user_id,role,version) VALUES ($1,$2,'member',1)`, projectID, principal.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetProjectBudget(ctx, projectOwner.ID, projectID,
		project.Budget{Currency: "USD", LimitMicros: 150, Version: 1}, 0, "budget-create-01"); err != nil {
		t.Fatal(err)
	}
	rate := project.RateSnapshot{Provider: "test-provider", ModelID: accountCall.ModelID, Model: accountCall.ProviderModel,
		Currency: "USD", Version: 7, InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 1_000_000}
	makeReservation := func(callID string) app.ProjectModelReservation {
		t.Helper()
		call := accountCall
		call.ProjectID = projectID
		call.CallID = callID
		accountDigest := sha256.Sum256([]byte("account-request-" + callID))
		call.RequestDigest = accountDigest[:]
		projectDigest := sha256.Sum256([]byte("project-request-" + callID))
		return app.ProjectModelReservation{ProjectID: projectID, ActorID: principal.UserID, Source: app.ModelCallClient,
			CallID: callID, ModelID: accountCall.ModelID, Usage: project.Usage{InputTokens: 80, OutputTokens: 20},
			ReservedTokens: 100, Rate: rate, ReservedMicros: 100, RequestDigest: projectDigest[:], AccountCall: call}
	}
	badAccountTokens := makeReservation("project-invalid-001")
	badAccountTokens.AccountCall.ReservedTokens++
	if _, _, err := store.ReserveProjectModelCall(ctx, badAccountTokens); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("mismatched account token reservation = %v, want invalid argument", err)
	}
	badProviderModel := makeReservation("project-invalid-002")
	badProviderModel.AccountCall.ProviderModel = "other-provider-model"
	if _, _, err := store.ReserveProjectModelCall(ctx, badProviderModel); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("mismatched account provider model = %v, want invalid argument", err)
	}
	type reserveResult struct {
		call app.ProjectModelReservation
		err  error
	}
	start := make(chan struct{})
	results := make(chan reserveResult, 2)
	for _, callID := range []string{"project-call-0001", "project-call-0002"} {
		call := makeReservation(callID)
		go func() {
			<-start
			_, _, err := store.ReserveProjectModelCall(ctx, call)
			results <- reserveResult{call: call, err: err}
		}()
	}
	close(start)
	first := <-results
	second := <-results
	reserved, denied := 0, 0
	var winner app.ProjectModelReservation
	for _, result := range []reserveResult{first, second} {
		if result.err == nil {
			reserved++
			winner = result.call
		} else if domain.CodeOf(result.err) == domain.CodeQuotaExceeded {
			denied++
		} else {
			t.Fatalf("concurrent project reserve = %v", result.err)
		}
	}
	if reserved != 1 || denied != 1 {
		t.Fatalf("project budget admitted %d reservations and denied %d; want 1/1", reserved, denied)
	}
	if _, err := store.SetProjectBudget(ctx, projectOwner.ID, projectID,
		project.Budget{Currency: "USD", LimitMicros: 200, Version: 2}, 1, "budget-update-02"); err != nil {
		t.Fatalf("raise budget with an active reservation: %v", err)
	}
	var projectRows, accountRows int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM project_budget_ledger WHERE project_id=$1`, projectID).Scan(&projectRows); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM model_call_ledger WHERE owner_id=$1 AND source='client'`, principal.UserID).Scan(&accountRows); err != nil {
		t.Fatal(err)
	}
	if projectRows != 1 || accountRows != 1 {
		t.Fatalf("account/project ledgers were not atomic: project=%d account=%d", projectRows, accountRows)
	}
	if _, err := store.pool.Exec(ctx, `DELETE FROM project_members WHERE project_id=$1 AND user_id=$2`, projectID, principal.UserID); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginModelCallDispatch(ctx, winner.AccountCall); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("dispatch after member revocation = %v, want denied", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO project_members(project_id,user_id,role,version) VALUES ($1,$2,'member',2)`, projectID, principal.UserID); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginModelCallDispatch(ctx, winner.AccountCall); err != nil {
		t.Fatalf("dispatch after authorized membership restore: %v", err)
	}
	settlement := app.SettleProjectModelCallCommand{ProjectID: projectID, Source: app.ModelCallClient, CallID: winner.CallID,
		Usage: project.Usage{InputTokens: 30, OutputTokens: 20}}
	if err := store.SettleProjectModelCall(ctx, settlement); err != nil {
		t.Fatal(err)
	}
	if err := store.SettleProjectModelCall(ctx, settlement); err != nil {
		t.Fatalf("same project settlement replay: %v", err)
	}

	secondCall := makeReservation("project-call-0003")
	secondCall.Rate.Version = 8
	secondCall.Rate.InputMicrosPerMillion = 1_100_000
	secondCall.Rate.OutputMicrosPerMillion = 1_200_000
	secondCall.ReservedMicros = 112
	if _, _, err := store.ReserveProjectModelCall(ctx, secondCall); err != nil {
		t.Fatalf("reserve after completed call: %v", err)
	}
	if err := store.SettleProjectModelCall(ctx, app.SettleProjectModelCallCommand{ProjectID: projectID,
		Source: app.ModelCallClient, CallID: secondCall.CallID, Unknown: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReserveProjectModelCall(ctx, makeReservation("project-call-0004")); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("project unknown usage did not hold budget: %v", err)
	}
	if err := store.SettleProjectModelCall(ctx, app.SettleProjectModelCallCommand{ProjectID: projectID,
		Source: app.ModelCallClient, CallID: secondCall.CallID, Usage: project.Usage{InputTokens: 20, OutputTokens: 10}}); err != nil {
		t.Fatalf("late project usage reconciliation: %v", err)
	}
	thirdCall := makeReservation("project-call-0004")
	thirdCall.Rate.Version = 9
	thirdCall.Rate.InputMicrosPerMillion = 1_100_000
	thirdCall.Rate.OutputMicrosPerMillion = 1_200_000
	thirdCall.ReservedMicros = 112
	if _, _, err := store.ReserveProjectModelCall(ctx, thirdCall); err != nil {
		t.Fatalf("reserve overage case: %v", err)
	}
	if err := store.BeginModelCallDispatch(ctx, thirdCall.AccountCall); err != nil {
		t.Fatalf("begin overage case: %v", err)
	}
	if err := store.SettleProjectModelCall(ctx, app.SettleProjectModelCallCommand{ProjectID: projectID,
		Source: app.ModelCallClient, CallID: thirdCall.CallID, Usage: project.Usage{InputTokens: 80, OutputTokens: 21}}); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("over-reservation usage = %v, want quota exceeded with holds retained", err)
	}
	if _, _, err := store.ReserveProjectModelCall(ctx, makeReservation("project-call-0005")); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("over-reservation did not hold both budgets: %v", err)
	}
	if err := store.SettleProjectModelCall(ctx, app.SettleProjectModelCallCommand{ProjectID: projectID,
		Source: app.ModelCallClient, CallID: thirdCall.CallID, Usage: project.Usage{InputTokens: 20, OutputTokens: 10}}); err != nil {
		t.Fatalf("recover over-reservation usage: %v", err)
	}
	view, err := store.GetProjectBudget(ctx, projectOwner.ID, projectID)
	if err != nil || view.SpentMicros != 118 || view.ReservedMicros != 0 || view.HasUnknown {
		t.Fatalf("project totals after recovery = %#v, %v", view, err)
	}
	var rates int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM project_budget_ledger WHERE project_id=$1 AND rate_version IN (7,8,9)`, projectID).Scan(&rates); err != nil || rates != 3 {
		t.Fatalf("per-call rate snapshots were not retained: count=%d, %v", rates, err)
	}
	personal := accountCall
	personal.ProjectID = ""
	personal.CallID = "personal-call-0001"
	personalDigest := sha256.Sum256([]byte("personal-request"))
	personal.RequestDigest = personalDigest[:]
	if err := store.ReserveModelCall(ctx, personal); err != nil {
		t.Fatalf("personal call after project use: %v", err)
	}
	if err := store.SettleModelCall(ctx, app.ModelCallSettlement{Source: app.ModelCallClient, OwnerID: principal.UserID,
		CallID: personal.CallID, Usage: app.ModelUsage{Known: true, InputTokens: 7, OutputTokens: 3}}); err != nil {
		t.Fatalf("personal model settlement: %v", err)
	}
	view, err = store.GetProjectBudget(ctx, projectOwner.ID, projectID)
	if err != nil || view.SpentMicros != 118 || view.ReservedMicros != 0 || view.HasUnknown {
		t.Fatalf("personal model use leaked into project totals: %#v, %v", view, err)
	}
	var accountState string
	if err := store.pool.QueryRow(ctx, `SELECT state FROM model_call_ledger WHERE owner_id=$1 AND source='client' AND call_id=$2`,
		principal.UserID, secondCall.CallID).Scan(&accountState); err != nil || accountState != "settled" {
		t.Fatalf("account ledger after project recovery = %q, %v", accountState, err)
	}
}

func TestPostgresProjectTaskModelReservationUsesPersistedScopeAndInitiator(t *testing.T) {
	store, principal, accountCall := modelUsageFixture(t)
	ctx := context.Background()
	if _, err := store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, principal.UserID); err != nil {
		t.Fatal(err)
	}
	projectOwner := createProjectBudgetTestUser(t, store)
	projectID, err := project.NewID()
	if err != nil {
		t.Fatal(err)
	}
	value, err := project.New(projectID, projectOwner.ID, "Task budget", "persisted Task scope attribution")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateProject(ctx, projectOwner.ID, value, "task-project-create-01"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO project_members(project_id,user_id,role,version) VALUES ($1,$2,'member',1)`, projectID, principal.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetProjectBudget(ctx, projectOwner.ID, projectID,
		project.Budget{Currency: "USD", LimitMicros: 500, Version: 1}, 0, "task-budget-create-01"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO conversations(id,owner_id,title,project_id) VALUES ('c1234567',$1,'project Task',$2)`, principal.UserID, projectID); err != nil {
		t.Fatal(err)
	}
	const taskID = "q1234567"
	checkpointDigest := sha256.Sum256([]byte("project-task-checkpoint"))
	_, err = store.pool.Exec(ctx, `INSERT INTO tasks(id,owner_id,initiator_id,project_id,conversation_id,model_id,objective,status,auth_kind,auth_token_id,
 auth_client_installation_id,budget_active_seconds,budget_tool_calls,budget_model_tokens,usage_model_tokens,
 lease_owner,lease_version,lease_expires_at,execution_location,auth_version,auth_expires_at,agent_instance_id,
 agent_generation,agent_session_id,agent_engine_version,agent_profile_version,checkpoint_through_seq,checkpoint_digest)
 VALUES ($1,$2,$2,$3,'c1234567','p1234567','project task','running','client',$4,$5,600,10,125,0,
 'worker-1',3,clock_timestamp()+interval '1 hour','server',7,clock_timestamp()+interval '1 hour',
 'agent-1',2,'session-1','engine','profile',0,$6)`, taskID, principal.UserID, projectID, principal.TokenID,
		principal.ClientInstallationID, checkpointDigest[:])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_agent_sessions(task_id,owner_id,session_id,checkpoint_through_seq,checkpoint_digest)
VALUES ($1,$2,'session-1',0,$3)`, taskID, principal.UserID, checkpointDigest[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_agent_instances
(task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,engine_version,profile_version,supervisor_id,
 stop_nonce_digest,lease_owner,lease_version,auth_version,initial_lease_ms,lease_expires_at,state)
VALUES ($1,$2,'agent-1',2,'session-1','engine','profile','supervisor-1',$3,'worker-1',3,7,30000,
 clock_timestamp()+interval '30 seconds','active')`, taskID, principal.UserID, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_agent_checkpoints(task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,agent_root_session_id,
 from_seq,through_seq,digest,ciphertext,engine_version,profile_version,auth_version,lease_version)
 VALUES ($1,$2,'agent-1',2,'session-1','session-1',0,0,$3,decode('01','hex'),'engine','profile',7,3)`,
		taskID, principal.UserID, checkpointDigest[:]); err != nil {
		t.Fatal(err)
	}
	fence := app.ModelTaskFence{TaskID: taskID, OwnerID: principal.UserID, InitiatorID: principal.UserID, LeaseOwner: "worker-1", LeaseVersion: 3,
		AgentInstanceID: "agent-1", AgentGeneration: 2, AgentSessionID: "session-1", AuthorizationVersion: 7, ThroughSequence: 0,
		EngineVersion: "engine", ProfileVersion: "profile"}
	callID, err := app.DSHModelCallID(taskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	accountCall.Source, accountCall.ProjectID, accountCall.Task, accountCall.Principal = app.ModelCallTask, projectID, &fence, identity.Principal{}
	accountCall.CallID, accountCall.OwnerID = callID, principal.UserID
	accountDigest := sha256.Sum256([]byte("project-task-account-request"))
	accountCall.RequestDigest = accountDigest[:]
	accountCall.ReservedTokens = 100
	rate := project.RateSnapshot{Provider: "test-provider", ModelID: accountCall.ModelID, Model: accountCall.ProviderModel,
		Currency: "USD", Version: 7, InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 1_000_000}
	projectDigest := sha256.Sum256([]byte("project-task-request"))
	reservation := app.ProjectModelReservation{ProjectID: projectID, ActorID: principal.UserID, TaskID: taskID, Source: app.ModelCallTask,
		CallID: callID, ModelID: accountCall.ModelID, Usage: project.Usage{InputTokens: 90, OutputTokens: 10},
		ReservedTokens: 100, Rate: rate, ReservedMicros: 100, RequestDigest: projectDigest[:], AccountCall: accountCall}
	if _, _, err := store.ReserveProjectModelCall(ctx, reservation); err != nil {
		t.Fatalf("reserve persisted project Task call: %v", err)
	}
	if err := store.BeginModelCallDispatch(ctx, accountCall); err != nil {
		t.Fatalf("begin persisted project Task call: %v", err)
	}
	settlement := app.SettleProjectModelCallCommand{ProjectID: projectID, Source: app.ModelCallTask, CallID: callID,
		Usage: project.Usage{InputTokens: 40, OutputTokens: 8}}
	if err := store.SettleProjectModelCall(ctx, settlement); err != nil {
		t.Fatalf("settle persisted project Task call: %v", err)
	}
	var projectActor, projectTask, projectState string
	var actualMicros int64
	if err := store.pool.QueryRow(ctx, `SELECT actor_id,task_id,status,actual_micros FROM project_budget_ledger
WHERE project_id=$1 AND source='task' AND call_id=$2`, projectID, callID).
		Scan(&projectActor, &projectTask, &projectState, &actualMicros); err != nil {
		t.Fatal(err)
	}
	var accountTask, accountState string
	var accountInput, accountOutput int64
	if err := store.pool.QueryRow(ctx, `SELECT task_id,state,input_tokens,output_tokens FROM model_call_ledger
WHERE owner_id=$1 AND source='task' AND call_id=$2`, principal.UserID, callID).
		Scan(&accountTask, &accountState, &accountInput, &accountOutput); err != nil {
		t.Fatal(err)
	}
	var taskProject, taskInitiator string
	var taskUsage int64
	if err := store.pool.QueryRow(ctx, `SELECT project_id,initiator_id,usage_model_tokens FROM tasks WHERE id=$1`, taskID).
		Scan(&taskProject, &taskInitiator, &taskUsage); err != nil {
		t.Fatal(err)
	}
	if projectActor != principal.UserID || projectTask != taskID || projectState != "settled" || actualMicros != 48 ||
		accountTask != taskID || accountState != "settled" || accountInput != 40 || accountOutput != 8 ||
		taskProject != projectID || taskInitiator != principal.UserID || taskUsage != 48 {
		t.Fatalf("Task dual-ledger attribution: project=(%s,%s,%s,%d) account=(%s,%s,%d,%d) task=(%s,%s,%d)",
			projectActor, projectTask, projectState, actualMicros, accountTask, accountState, accountInput, accountOutput,
			taskProject, taskInitiator, taskUsage)
	}

	var personalVersion time.Time
	if err := store.pool.QueryRow(ctx, `INSERT INTO model_connections(id,owner_id,name,base_url,model_name,api_key_cipher,
capability_text,capability_tools,checked_at) VALUES ('p7654321',$1,'personal project model','https://model.example/v1','test-model',
decode(repeat('01',29),'hex'),true,true,now()) RETURNING updated_at`, principal.UserID).Scan(&personalVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET model_id='p7654321',budget_model_tokens=1000 WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `DELETE FROM model_account_budgets WHERE owner_id=$1`, principal.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `DELETE FROM project_budgets WHERE project_id=$1`, projectID); err != nil {
		t.Fatal(err)
	}
	personal := accountCall
	personal.ModelID, personal.ConnectionVersion, personal.PersonalModel, personal.CallID = "p7654321", personalVersion, true, "personal-project-call-01"
	if err := store.ReserveModelCall(ctx, personal); err != nil {
		t.Fatalf("personal project model without account or project budget: %v", err)
	}
	if err := store.BeginModelCallDispatch(ctx, personal); err != nil {
		t.Fatalf("personal project model dispatch: %v", err)
	}
	if err := store.SettleModelCall(ctx, app.ModelCallSettlement{Source: personal.Source, OwnerID: personal.OwnerID,
		CallID: personal.CallID, Usage: app.ModelUsage{Known: true, InputTokens: 10, OutputTokens: 5}}); err != nil {
		t.Fatalf("personal project model settlement: %v", err)
	}
	var projectCharges int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM project_budget_ledger WHERE project_id=$1 AND call_id=$2`, projectID, personal.CallID).Scan(&projectCharges); err != nil || projectCharges != 0 {
		t.Fatalf("personal model charged project budget: %d, %v", projectCharges, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT usage_model_tokens FROM tasks WHERE id=$1`, taskID).Scan(&taskUsage); err != nil || taskUsage != 63 {
		t.Fatalf("personal model was not included in task usage: %d, %v", taskUsage, err)
	}
	stale := personal
	stale.CallID = "personal-project-stale-01"
	if err := store.ReserveModelCall(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE model_call_ledger SET created_at=clock_timestamp()-interval '3 minutes'
WHERE owner_id=$1 AND source=$2 AND call_id=$3`, stale.OwnerID, stale.Source, stale.CallID); err != nil {
		t.Fatal(err)
	}
	if marked, err := store.MarkStaleModelCallsUnknown(ctx); err != nil || marked != 1 {
		t.Fatalf("recover personal project call without budgets: marked=%d err=%v", marked, err)
	}
	admin := personalQuotaAdminPrincipal(t, store)
	adminService, err := app.NewModelUsageAdminService(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adminService.Reconcile(ctx, admin, app.ModelUsageReconcileCommand{OwnerID: principal.UserID,
		Source: stale.Source, CallID: stale.CallID, InputTokens: 4, OutputTokens: 2, Evidence: "provider reference"}); err != nil {
		t.Fatalf("reconcile personal project usage without budgets: %v", err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT usage_model_tokens FROM tasks WHERE id=$1`, taskID).Scan(&taskUsage); err != nil || taskUsage != 69 {
		t.Fatalf("reconciled personal project task usage: %d, %v", taskUsage, err)
	}
}

func TestPostgresProjectKnownSettlementFailureLeavesBothHoldsUnknown(t *testing.T) {
	store, principal, accountCall := modelUsageFixture(t)
	ctx := context.Background()
	projectOwner := createProjectBudgetTestUser(t, store)
	projectID, err := project.NewID()
	if err != nil {
		t.Fatal(err)
	}
	value, err := project.New(projectID, projectOwner.ID, "Settlement failure", "hold both ledgers if charge write fails")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateProject(ctx, projectOwner.ID, value, "settle-project-create-01"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO project_members(project_id,user_id,role,version) VALUES ($1,$2,'member',1)`, projectID, principal.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetProjectBudget(ctx, projectOwner.ID, projectID,
		project.Budget{Currency: "USD", LimitMicros: 500, Version: 1}, 0, "settle-budget-create-01"); err != nil {
		t.Fatal(err)
	}
	rate := project.RateSnapshot{Provider: "test-provider", ModelID: accountCall.ModelID, Model: accountCall.ProviderModel,
		Currency: "USD", Version: 7, InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 1_000_000}
	accountCall.ProjectID, accountCall.CallID = projectID, "settle-failure-001"
	accountDigest := sha256.Sum256([]byte("account-settle-failure-request"))
	accountCall.RequestDigest = accountDigest[:]
	reservationDigest := sha256.Sum256([]byte("project-settle-failure-request"))
	reservation := app.ProjectModelReservation{ProjectID: projectID, ActorID: principal.UserID, Source: app.ModelCallClient,
		CallID: accountCall.CallID, ModelID: accountCall.ModelID, Usage: project.Usage{InputTokens: 90, OutputTokens: 10},
		ReservedTokens: 100, Rate: rate, ReservedMicros: 100, RequestDigest: reservationDigest[:], AccountCall: accountCall}
	if _, _, err := store.ReserveProjectModelCall(ctx, reservation); err != nil {
		t.Fatalf("reserve before forced settlement failure: %v", err)
	}
	if err := store.BeginModelCallDispatch(ctx, accountCall); err != nil {
		t.Fatalf("dispatch before forced settlement failure: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `CREATE FUNCTION reject_project_budget_settlement() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.status='settled' THEN RAISE EXCEPTION 'forced project settlement failure'; END IF;
  RETURN NEW;
END;

$$`); err != nil {
		t.Fatalf("install settlement failure trigger: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `CREATE TRIGGER reject_project_budget_settlement BEFORE UPDATE ON project_budget_ledger
FOR EACH ROW EXECUTE FUNCTION reject_project_budget_settlement()`); err != nil {
		t.Fatalf("install settlement failure trigger: %v", err)
	}
	settlement := app.SettleProjectModelCallCommand{ProjectID: projectID, Source: app.ModelCallClient, CallID: accountCall.CallID,
		Usage: project.Usage{InputTokens: 40, OutputTokens: 8}}
	if err := store.SettleProjectModelCall(ctx, settlement); err == nil {
		t.Fatal("known settlement unexpectedly succeeded despite forced project-ledger failure")
	}
	var projectState, accountState string
	if err := store.pool.QueryRow(ctx, `SELECT status FROM project_budget_ledger WHERE project_id=$1 AND source='client' AND call_id=$2`,
		projectID, accountCall.CallID).Scan(&projectState); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT state FROM model_call_ledger WHERE owner_id=$1 AND source='client' AND call_id=$2`,
		principal.UserID, accountCall.CallID).Scan(&accountState); err != nil {
		t.Fatal(err)
	}
	if projectState != "unknown" || accountState != "unknown" {
		t.Fatalf("failed known settlement did not retain both holds: project=%s account=%s", projectState, accountState)
	}
	if _, err := store.pool.Exec(ctx, `DROP TRIGGER reject_project_budget_settlement ON project_budget_ledger`); err != nil {
		t.Fatalf("remove settlement failure trigger: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `DROP FUNCTION reject_project_budget_settlement()`); err != nil {
		t.Fatalf("remove settlement failure trigger: %v", err)
	}
	if err := store.SettleProjectModelCall(ctx, settlement); err != nil {
		t.Fatalf("reconcile failed settlement after removing trigger: %v", err)
	}
	if err := store.bestEffortUnknownProjectModelCall(ctx, settlement, principal.UserID); err != nil {
		t.Fatalf("unknown fallback against successful settled pair: %v", err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT status FROM project_budget_ledger WHERE project_id=$1 AND source='client' AND call_id=$2`,
		projectID, accountCall.CallID).Scan(&projectState); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT state FROM model_call_ledger WHERE owner_id=$1 AND source='client' AND call_id=$2`,
		principal.UserID, accountCall.CallID).Scan(&accountState); err != nil {
		t.Fatal(err)
	}
	if projectState != "settled" || accountState != "settled" {
		t.Fatalf("unknown fallback overwrote a successful charge: project=%s account=%s", projectState, accountState)
	}
}

func newProjectBudgetTestStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("CH_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("CH_TEST_DATABASE_URL is unset; project budget PostgreSQL integration test was not run")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	baseConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse CH_TEST_DATABASE_URL: %v", err)
	}
	adminPool, err := pgxpool.NewWithConfig(ctx, baseConfig.Copy())
	if err != nil {
		t.Fatalf("connect to test PostgreSQL server: %v", err)
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	databaseName := "morde_project_budget_" + hex.EncodeToString(random[:])
	if _, err := adminPool.Exec(ctx, fmt.Sprintf(`CREATE DATABASE "%s"`, databaseName)); err != nil {
		adminPool.Close()
		t.Fatalf("create temporary project budget database: %v", err)
	}

	testConfig := baseConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testPool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		_, _ = adminPool.Exec(ctx, fmt.Sprintf(`DROP DATABASE "%s" WITH (FORCE)`, databaseName))
		adminPool.Close()
		t.Fatalf("connect to temporary project budget database: %v", err)
	}
	if err := Migrate(ctx, testPool); err != nil {
		testPool.Close()
		_, _ = adminPool.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE "%s" WITH (FORCE)`, databaseName))
		adminPool.Close()
		t.Fatalf("migrate temporary project budget database: %v", err)
	}
	t.Cleanup(func() {
		testPool.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := adminPool.Exec(cleanupCtx, fmt.Sprintf(`DROP DATABASE "%s" WITH (FORCE)`, databaseName)); err != nil {
			t.Errorf("drop temporary project budget database: %v", err)
		}
		adminPool.Close()
	})
	return New(testPool), testPool
}

func createProjectBudgetTestUser(t *testing.T, store *Store) identity.User {
	t.Helper()
	id, err := domain.NewUserID()
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.CreateUser(context.Background(), identity.User{
		ID: id, Name: "user-" + id, PasswordHash: "hash", Role: identity.RoleUser,
		Status: identity.StatusPending, QuotaHosts: 4, QuotaCPUMilli: 20_000, QuotaMemMiB: 40_000,
		DefaultHostCPUMilli: 1000, DefaultHostMemMiB: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	user.Status = identity.StatusApproved
	user, err = store.UpdateUser(context.Background(), "system", identity.StatusPending, user)
	if err != nil {
		t.Fatal(err)
	}
	return user
}
