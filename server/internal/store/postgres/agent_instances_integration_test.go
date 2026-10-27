package postgres

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type agentLifecycleFixture struct {
	store     *Store
	service   *app.AgentInstanceService
	clients   *app.ClientLoginService
	principal identity.Principal
	request   app.AgentInstanceStart
	key       []byte
}

func newAgentLifecycleFixture(t *testing.T) agentLifecycleFixture {
	t.Helper()
	store, credentials, clients, owner := clientLoginFixture(t)
	ctx := context.Background()
	login, err := clients.Login(ctx, owner.Name, "client-password-123", "ios", "phone", "", "agent-lifecycle-login", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := credentials.VerifyClient(ctx, login.ClientCredential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	authExpires := time.Now().UTC().Add(time.Minute)
	submission := newTestTaskSubmission(t, owner.ID, "", "agent-lifecycle-task", "run without a Host")
	submission.Task.Authorization = task.Authorization{Kind: string(identity.KindClient), TokenID: principal.TokenID,
		ClientInstallationID: principal.ClientInstallationID, Version: 1, ExpiresAt: &authExpires}
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "lifecycle-worker", 25*time.Second)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim task: claimed=%t err=%v", found, err)
	}
	key := make([]byte, 32)
	key[0] = 23
	authenticator, err := app.NewAgentStopAuthenticator("supervisor-1", key)
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.NewAgentInstanceService(store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	nonce[0] = 1
	return agentLifecycleFixture{store: store, service: service, clients: clients, principal: principal, key: key,
		request: app.AgentInstanceStart{Fence: app.AgentInstanceFence{TaskID: claimed.ID, OwnerID: owner.ID,
			AgentInstanceID: "agent-1", AgentSessionID: "session-1", SupervisorID: "supervisor-1",
			LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version, AuthorizationVersion: 1,
			EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1"}, SupervisorNonce: nonce, LeaseDuration: 30 * time.Second}}
}

func signAgentLifecycleReceipt(t *testing.T, key []byte, fence app.AgentInstanceFence, nonce []byte) app.AgentStopReceipt {
	t.Helper()
	receipt := app.AgentStopReceipt{Fence: fence, SupervisorNonce: nonce, TreeStopped: true}
	payload, err := app.AgentStopReceiptSigningPayload(receipt)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	receipt.Signature = mac.Sum(nil)
	return receipt
}

func TestPostgresAgentLifecycleRequiresVerifiedStopBeforeReplacement(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	first, replayed, err := f.service.Start(ctx, f.request)
	if err != nil || replayed || first.Fence.AgentGeneration != 1 {
		t.Fatalf("start first agent: lease=%+v replayed=%t err=%v", first, replayed, err)
	}
	var taskExpiry, authExpiry time.Time
	if err := f.store.pool.QueryRow(ctx, `SELECT lease_expires_at,auth_expires_at FROM tasks WHERE id=$1`, first.Fence.TaskID).
		Scan(&taskExpiry, &authExpiry); err != nil {
		t.Fatal(err)
	}
	if first.ExpiresAt.After(taskExpiry) || first.ExpiresAt.After(authExpiry) || first.ExpiresAt.After(time.Now().Add(30*time.Second)) {
		t.Fatalf("agent lease exceeds its bounds: agent=%v task=%v auth=%v", first.ExpiresAt, taskExpiry, authExpiry)
	}
	again, replayed, err := f.service.Start(ctx, f.request)
	if err != nil || !replayed || again.Fence != first.Fence || !again.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatalf("replay agent start: lease=%+v replayed=%t err=%v", again, replayed, err)
	}
	changedStart := f.request
	changedStart.LeaseDuration = 20 * time.Second
	if _, _, err := f.service.Start(ctx, changedStart); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("start replay accepted a changed lease: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_agent_instances SET lease_expires_at=clock_timestamp()-interval '1 second'
WHERE task_id=$1 AND agent_generation=1`, first.Fence.TaskID); err != nil {
		t.Fatal(err)
	}
	next := f.request
	next.Fence.AgentInstanceID = "agent-2"
	next.SupervisorNonce = make([]byte, 32)
	next.SupervisorNonce[0] = 2
	if _, _, err := f.service.Start(ctx, next); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("expired but unconfirmed agent was replaced: %v", err)
	}
	if _, err := f.service.Renew(ctx, first.Fence, time.Second); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("expired agent lease was revived: %v", err)
	}
	wrongNonce := signAgentLifecycleReceipt(t, f.key, first.Fence, next.SupervisorNonce)
	if _, err := f.service.ConfirmStopped(ctx, wrongNonce); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("receipt for another supervisor operation accepted: %v", err)
	}
	forged := signAgentLifecycleReceipt(t, make([]byte, 32), first.Fence, f.request.SupervisorNonce)
	if _, err := f.service.ConfirmStopped(ctx, forged); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("forged supervisor signature accepted: %v", err)
	}
	digest := sha256.Sum256([]byte("checkpoint"))
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_agent_checkpoints
(task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,from_seq,through_seq,digest,ciphertext,engine_version,profile_version,auth_version,lease_version)
VALUES ($1,$2,$3,1,$4,0,0,$5,'sealed',$6,$7,1,$8)`, first.Fence.TaskID, first.Fence.OwnerID,
		first.Fence.AgentInstanceID, first.Fence.AgentSessionID, digest[:], first.Fence.EngineVersion,
		first.Fence.ProfileVersion, first.Fence.LeaseVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET checkpoint_through_seq=0,checkpoint_digest=$2 WHERE id=$1`, first.Fence.TaskID, digest[:]); err != nil {
		t.Fatal(err)
	}
	receipt := signAgentLifecycleReceipt(t, f.key, first.Fence, f.request.SupervisorNonce)
	if replayed, err := f.service.ConfirmStopped(ctx, receipt); err != nil || replayed {
		t.Fatalf("confirm first agent stopped: replayed=%t err=%v", replayed, err)
	}
	var storedReceipt []byte
	if err := f.store.pool.QueryRow(ctx, `SELECT stop_receipt FROM task_agent_instances WHERE task_id=$1 AND agent_generation=1`, first.Fence.TaskID).
		Scan(&storedReceipt); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_agent_instances SET stop_receipt='altered' WHERE task_id=$1 AND agent_generation=1`, first.Fence.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.service.Start(ctx, next); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("replacement accepted corrupted stop evidence: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_agent_instances SET stop_receipt=$2 WHERE task_id=$1 AND agent_generation=1`, first.Fence.TaskID, storedReceipt); err != nil {
		t.Fatal(err)
	}
	badDigest := sha256.Sum256([]byte("missing checkpoint"))
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET checkpoint_digest=$2 WHERE id=$1`, first.Fence.TaskID, badDigest[:]); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.service.Start(ctx, next); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("replacement started without its checkpoint: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET checkpoint_digest=$2 WHERE id=$1`, first.Fence.TaskID, digest[:]); err != nil {
		t.Fatal(err)
	}
	second, replayed, err := f.service.Start(ctx, next)
	if err != nil || replayed || second.Fence.AgentGeneration != 2 || second.Fence.AgentSessionID != first.Fence.AgentSessionID {
		t.Fatalf("start replacement after verified stop: lease=%+v replayed=%t err=%v", second, replayed, err)
	}
	loaded, err := f.store.GetTask(ctx, first.Fence.TaskID, first.Fence.OwnerID)
	if err != nil || loaded.Task.CheckpointThroughSequence != 0 || loaded.Task.AgentInstanceID != "agent-2" ||
		string(loaded.Task.CheckpointDigest) != string(digest[:]) {
		t.Fatalf("replacement lost task session or checkpoint: task=%+v err=%v", loaded.Task, err)
	}
	if _, err := f.service.Renew(ctx, first.Fence, time.Second); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("old agent renewed after replacement: %v", err)
	}
	if replayed, err := f.service.ConfirmStopped(ctx, receipt); err != nil || !replayed {
		t.Fatalf("stop receipt retry failed: replayed=%t err=%v", replayed, err)
	}
	provider, closeProvider, err := newMigrationProvider(f.store.pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 57); err == nil {
		t.Fatal("rollback discarded agent lifecycle evidence")
	}
}

func TestPostgresAgentLeaseFollowsAuthorizationAndStillRecordsStop(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	first, _, err := f.service.Start(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET auth_expires_at=clock_timestamp()+interval '3 seconds' WHERE id=$1`, first.Fence.TaskID); err != nil {
		t.Fatal(err)
	}
	renewed, err := f.service.Renew(ctx, first.Fence, 30*time.Second)
	if err != nil || renewed.ExpiresAt.After(time.Now().Add(4*time.Second)) {
		t.Fatalf("renewed agent exceeded shortened task authorization: lease=%+v err=%v", renewed, err)
	}
	var taskExpiry time.Time
	if err := f.store.pool.QueryRow(ctx, `UPDATE tasks SET auth_expires_at=clock_timestamp()+interval '1 minute',
lease_expires_at=clock_timestamp()+interval '2 seconds' WHERE id=$1 RETURNING lease_expires_at`, first.Fence.TaskID).Scan(&taskExpiry); err != nil {
		t.Fatal(err)
	}
	renewed, err = f.service.Renew(ctx, first.Fence, 30*time.Second)
	if err != nil || renewed.ExpiresAt.After(taskExpiry) {
		t.Fatalf("renewed agent exceeded task lease: agent=%v task=%v err=%v", renewed.ExpiresAt, taskExpiry, err)
	}
	if err := f.clients.Logout(ctx, f.principal, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Renew(ctx, first.Fence, time.Second); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked installation renewed agent lease: %v", err)
	}
	receipt := signAgentLifecycleReceipt(t, f.key, first.Fence, f.request.SupervisorNonce)
	if _, err := f.service.ConfirmStopped(ctx, receipt); err != nil {
		t.Fatalf("supervisor could not record stop after revocation: %v", err)
	}
	if _, _, err := f.service.Start(ctx, f.request); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked installation started another agent: %v", err)
	}
}

func TestPostgresAgentConcurrentStartsAllowOnlyOneInstance(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	requests := []app.AgentInstanceStart{f.request, f.request}
	requests[1].Fence.AgentInstanceID = "agent-2"
	requests[1].SupervisorNonce = make([]byte, 32)
	requests[1].SupervisorNonce[0] = 2
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for _, request := range requests {
		wait.Add(1)
		go func(request app.AgentInstanceStart) {
			defer wait.Done()
			<-start
			_, _, err := f.service.Start(context.Background(), request)
			results <- err
		}(request)
	}
	close(start)
	wait.Wait()
	close(results)
	var successes, rejected int
	for err := range results {
		if err == nil {
			successes++
		} else if domain.CodeOf(err) == domain.CodeInvalidState || domain.CodeOf(err) == domain.CodeConflict {
			rejected++
		} else {
			t.Fatalf("unexpected concurrent start result: %v", err)
		}
	}
	var count int
	if err := f.store.pool.QueryRow(context.Background(), `SELECT count(*) FROM task_agent_instances WHERE task_id=$1`, f.request.Fence.TaskID).
		Scan(&count); err != nil || successes != 1 || rejected != 1 || count != 1 {
		t.Fatalf("concurrent starts: success=%d rejected=%d instances=%d err=%v", successes, rejected, count, err)
	}
}

func TestPostgresAgentRejectsMissingEvidenceAndHostIdentity(t *testing.T) {
	t.Run("missing prior instance record", func(t *testing.T) {
		f := newAgentLifecycleFixture(t)
		if _, err := f.store.pool.Exec(context.Background(), `UPDATE tasks SET agent_instance_id='lost-agent',agent_generation=1,
agent_session_id='session-1',agent_engine_version='0.2.0-rc.2',agent_profile_version='v1' WHERE id=$1`, f.request.Fence.TaskID); err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.service.Start(context.Background(), f.request); domain.CodeOf(err) != domain.CodeInvalidState {
			t.Fatalf("missing process evidence allowed replacement: %v", err)
		}
	})
	t.Run("agent cannot be Host ID", func(t *testing.T) {
		f := newAgentLifecycleFixture(t)
		hostValue := createTestHost(t, context.Background(), f.store, f.principal.UserID,
			app.Capacity{CPUMilli: 100000, MemMiB: 200000}, nil)
		if _, err := f.store.pool.Exec(context.Background(), `UPDATE tasks SET host_id=$2 WHERE id=$1`, f.request.Fence.TaskID, hostValue.ID); err != nil {
			t.Fatal(err)
		}
		f.request.Fence.AgentInstanceID = hostValue.ID
		if _, _, err := f.service.Start(context.Background(), f.request); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Fatalf("Host ID accepted as agent instance: %v", err)
		}
	})
}
