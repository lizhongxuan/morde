package postgres

import (
	"context"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	domainhost "morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

func TestPostgresTokenAndInstallationRevocationStopsOnlyMatchingTasks(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	other := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)

	sessionTask, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "revoked-session", "session task"))
	if err != nil {
		t.Fatal(err)
	}
	installationID := createTestClientInstallation(t, store.pool, owner.ID, nil)
	clientToken := clientTestToken(t, owner.ID, installationID, identity.KindClient, nil)
	if _, err := store.CreateToken(ctx, clientToken); err != nil {
		t.Fatal(err)
	}
	clientSubmission := newTestTaskSubmission(t, owner.ID, "", "revoked-install", "client task")
	clientSubmission.Task.Authorization = task.Authorization{
		Kind: string(identity.KindClient), TokenID: clientToken.ID, ClientInstallationID: installationID,
		Version: 1, ExpiresAt: &clientToken.ExpiresAt,
	}
	clientTask, err := store.SubmitTask(ctx, clientSubmission)
	if err != nil {
		t.Fatal(err)
	}
	otherTask, err := store.SubmitTask(ctx, newTestTaskSubmission(t, other.ID, "", "unrelated-owner", "unrelated task"))
	if err != nil {
		t.Fatal(err)
	}

	if err := store.RevokeToken(ctx, testSessionTokenID(owner.ID), owner.ID); err != nil {
		t.Fatalf("revoke session token: %v", err)
	}
	assertTaskStatus(t, ctx, store, sessionTask.Task.ID, task.StatusCancelling)
	assertTaskStatus(t, ctx, store, clientTask.Task.ID, task.StatusQueued)
	assertTaskStatus(t, ctx, store, otherTask.Task.ID, task.StatusQueued)

	if err := store.RevokeClientInstallation(ctx, installationID, owner.ID, "127.0.0.1", audit.ActorSession, nil); err != nil {
		t.Fatalf("revoke client installation: %v", err)
	}
	assertTaskStatus(t, ctx, store, clientTask.Task.ID, task.StatusCancelling)
	assertTaskStatus(t, ctx, store, otherTask.Task.ID, task.StatusQueued)

	claimed, found, err := store.ClaimNextTask(ctx, "revocation-scope-worker", time.Minute)
	if err != nil || !found || claimed.ID != otherTask.Task.ID {
		t.Fatalf("claim unrelated task after revocation: task=%+v found=%t err=%v", claimed, found, err)
	}
	var pendingStops int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM task_stop_requests WHERE task_id IN ($1,$2) AND action='cancel' AND status='pending'`,
		sessionTask.Task.ID, clientTask.Task.ID).Scan(&pendingStops); err != nil || pendingStops != 2 {
		t.Fatalf("pending scoped cancellation requests = %d, err=%v", pendingStops, err)
	}
}

func TestPostgresSessionRevocationFencesTaskOperationAndLease(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, domainhost.ControlAI, 0)
	const runtimeInstanceID = "session-revoke-task-runtime"
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_instance_id=$2 WHERE id=$1`, hostValue.ID, runtimeInstanceID); err != nil {
		t.Fatal(err)
	}
	submitted, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "session-revoke-dispatch", "run task"))
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "session-revoke-worker", time.Minute)
	if err != nil || !found || claimed.ID != submitted.Task.ID {
		t.Fatalf("claim task before revocation: task=%+v found=%t err=%v", claimed, found, err)
	}
	operation, _, err := store.CreateOperation(ctx, task.Operation{
		ID: mustID(t, domain.NewOperationID), TaskID: claimed.ID, Key: "browser.click", RequestHash: hashBytes("click"),
		HostID: hostValue.ID, RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: runtimeInstanceID,
		ControlVersion: 0, LeaseVersion: claimed.Lease.Version, RuntimeLeaseVersion: claimed.Lease.Version, Status: task.OperationPrepared,
	}, claimed.Lease.Owner)
	if err != nil {
		t.Fatalf("create prepared Host operation: %v", err)
	}

	if err := store.RevokeToken(ctx, testSessionTokenID(owner.ID), owner.ID); err != nil {
		t.Fatalf("revoke task session token: %v", err)
	}
	if _, err := store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: claimed.ID, OperationKey: operation.Key, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: runtimeInstanceID, ControlVersion: 0,
		RuntimeLeaseVersion: claimed.Lease.Version, Status: task.OperationDispatched,
	}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("Host operation dispatch after session revocation = %v", err)
	}
	if _, err := store.RenewTaskLease(ctx, claimed.ID, claimed.Lease.Owner, claimed.Lease.Version, time.Minute); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("task lease renewal after session revocation = %v", err)
	}
	loaded, err := store.GetOperation(ctx, owner.ID, claimed.ID, operation.Key)
	if err != nil || loaded.Status != task.OperationPrepared {
		t.Fatalf("operation after rejected dispatch = %#v err=%v", loaded, err)
	}
	assertTaskStatus(t, ctx, store, claimed.ID, task.StatusCancelling)
}

func TestPostgresHostMemberRevocationCancelsOnlyMemberMCPWork(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	member := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue, fence := createRevocationMCPHost(t, ctx, store, owner.ID, "member-revoke-runtime")
	ensureMCPTestAPIKey(t, ctx, store, member.ID, "t1234567")
	ensureMCPTestAPIKey(t, ctx, store, owner.ID, "t7654321")
	if _, err := store.UpsertHostMember(ctx, domainhost.Member{
		HostID: hostValue.ID, OwnerID: owner.ID, UserID: member.ID, Permissions: []domainhost.Permission{domainhost.PermissionMCPControl},
	}); err != nil {
		t.Fatal(err)
	}
	memberKey := app.MCPRuntimeOperationKey{OwnerID: owner.ID, ActorUserID: member.ID, APIKeyID: "t1234567",
		SessionID: "member-session", HostID: hostValue.ID, IdempotencyKey: "member-operation"}
	ownerKey := app.MCPRuntimeOperationKey{OwnerID: owner.ID, APIKeyID: "t7654321",
		SessionID: "owner-session", HostID: hostValue.ID, IdempotencyKey: "owner-operation"}
	memberOperation, _, err := store.BeginMCPRuntimeOperation(ctx, newMCPRuntimeBeginRequest(t, memberKey, fence, 1))
	if err != nil {
		t.Fatalf("begin member operation: %v", err)
	}
	ownerOperation, _, err := store.BeginMCPRuntimeOperation(ctx, newMCPRuntimeBeginRequest(t, ownerKey, fence, 1))
	if err != nil {
		t.Fatalf("begin owner operation: %v", err)
	}

	if err := store.RevokeHostMember(ctx, hostValue.ID, owner.ID, member.ID); err != nil {
		t.Fatalf("revoke Host member: %v", err)
	}
	assertMCPRuntimeCancellation(t, ctx, store, memberOperation.OperationID, true)
	assertMCPRuntimeCancellation(t, ctx, store, ownerOperation.OperationID, false)
	if _, err := store.MarkMCPRuntimeOperationDispatched(ctx, memberKey, memberOperation.RuntimeTaskID, fence, memberOperation.LeaseVersion); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked member dispatched MCP operation: %v", err)
	}
	if _, err := store.MarkMCPRuntimeOperationDispatched(ctx, ownerKey, ownerOperation.RuntimeTaskID, fence, ownerOperation.LeaseVersion); err != nil {
		t.Fatalf("unrelated owner MCP operation was blocked: %v", err)
	}
}

func TestPostgresMCPTokenRevocationRacesDispatch(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	ensureMCPTestAPIKey(t, ctx, store, owner.ID, "t1234567")
	hostValue, fence := createRevocationMCPHost(t, ctx, store, owner.ID, "token-revoke-runtime")
	key := app.MCPRuntimeOperationKey{OwnerID: owner.ID, APIKeyID: "t1234567", SessionID: "race-session",
		HostID: hostValue.ID, IdempotencyKey: "race-operation"}
	operation, _, err := store.BeginMCPRuntimeOperation(ctx, newMCPRuntimeBeginRequest(t, key, fence, 1))
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	var wait sync.WaitGroup
	results := make(chan error, 2)
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		_, err := store.MarkMCPRuntimeOperationDispatched(ctx, key, operation.RuntimeTaskID, fence, operation.LeaseVersion)
		results <- err
	}()
	go func() {
		defer wait.Done()
		<-start
		results <- store.RevokeToken(ctx, key.APIKeyID, owner.ID)
	}()
	close(start)
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil && domain.CodeOf(err) != domain.CodeUnauthenticated && domain.CodeOf(err) != domain.CodeConflict {
			t.Fatalf("racing revoke or dispatch failed unexpectedly: %v", err)
		}
	}

	if _, err := store.MarkMCPRuntimeOperationDispatched(ctx, key, operation.RuntimeTaskID, fence, operation.LeaseVersion); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("dispatch after committed token revocation = %v", err)
	}
	assertMCPRuntimeCancellation(t, ctx, store, operation.OperationID, true)
}

func TestPostgresAccountDisableStopsOwnedTasksAndMCPWork(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	taskValue, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "disabled-owner-task", "stop after disable"))
	if err != nil {
		t.Fatal(err)
	}
	ensureMCPTestAPIKey(t, ctx, store, owner.ID, "t1234567")
	hostValue, fence := createRevocationMCPHost(t, ctx, store, owner.ID, "disabled-owner-runtime")
	key := app.MCPRuntimeOperationKey{OwnerID: owner.ID, APIKeyID: "t1234567", SessionID: "disabled-owner-session",
		HostID: hostValue.ID, IdempotencyKey: "disabled-owner-operation"}
	operation, _, err := store.BeginMCPRuntimeOperation(ctx, newMCPRuntimeBeginRequest(t, key, fence, 1))
	if err != nil {
		t.Fatal(err)
	}

	owner.Status = identity.StatusDisabled
	if _, err := store.UpdateUser(ctx, "system", identity.StatusApproved, owner); err != nil {
		t.Fatalf("disable account: %v", err)
	}
	assertTaskStatus(t, ctx, store, taskValue.Task.ID, task.StatusCancelling)
	assertMCPRuntimeCancellation(t, ctx, store, operation.OperationID, true)
	if _, found, err := store.ClaimNextTask(ctx, "disabled-account-worker", time.Minute); err != nil || found {
		t.Fatalf("disabled account task was claimable: found=%t err=%v", found, err)
	}
	if _, err := store.MarkMCPRuntimeOperationDispatched(ctx, key, operation.RuntimeTaskID, fence, operation.LeaseVersion); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("disabled account dispatched MCP operation: %v", err)
	}
}

func createRevocationMCPHost(t *testing.T, ctx context.Context, store *Store, ownerID, runtimeID string) (domainhost.Host, app.AgentHostFence) {
	t.Helper()
	hostValue := createRunningControlHost(t, ctx, store, ownerID, domainhost.ControlAI, 0)
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_generation=1, runtime_instance_id=$2 WHERE id=$1`, hostValue.ID, runtimeID); err != nil {
		t.Fatal(err)
	}
	return hostValue, app.AgentHostFence{RuntimeGeneration: 1, RuntimeInstanceID: runtimeID, ControlVersion: 0, AIAllowed: true}
}

func assertTaskStatus(t *testing.T, ctx context.Context, store *Store, taskID string, expected task.Status) {
	t.Helper()
	var status string
	if err := store.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, taskID).Scan(&status); err != nil || task.Status(status) != expected {
		t.Fatalf("task %s status = %q, err=%v; want %q", taskID, status, err, expected)
	}
}

func assertMCPRuntimeCancellation(t *testing.T, ctx context.Context, store *Store, operationID string, expected bool) {
	t.Helper()
	var requested bool
	if err := store.pool.QueryRow(ctx, `SELECT cancel_requested_at IS NOT NULL FROM mcp_runtime_operations WHERE operation_id=$1`, operationID).Scan(&requested); err != nil || requested != expected {
		t.Fatalf("operation %s cancel_requested = %t, err=%v; want %t", operationID, requested, err, expected)
	}
}
