package postgres

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/control"
	"morde/server/internal/domain/host"
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
	login, err := clientLoginForTest(ctx, clients, owner.Name, "client-password-123", "ios", "phone", "", "agent-lifecycle-login", "127.0.0.1")
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
		request: app.AgentInstanceStart{Fence: app.AgentInstanceFence{TaskID: claimed.ID, OwnerID: owner.ID, InitiatorID: claimed.InitiatorID,
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
(task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,agent_root_session_id,from_seq,through_seq,digest,ciphertext,engine_version,profile_version,auth_version,lease_version)
VALUES ($1,$2,$3,1,$4,$4,0,0,$5,'sealed',$6,$7,1,$8)`, first.Fence.TaskID, first.Fence.OwnerID,
		first.Fence.AgentInstanceID, first.Fence.AgentSessionID, digest[:], first.Fence.EngineVersion,
		first.Fence.ProfileVersion, first.Fence.LeaseVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET checkpoint_through_seq=0,checkpoint_digest=$2 WHERE id=$1`, first.Fence.TaskID, digest[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_agent_sessions SET checkpoint_through_seq=0,checkpoint_digest=$3
WHERE task_id=$1 AND session_id=$2`, first.Fence.TaskID, first.Fence.AgentSessionID, digest[:]); err != nil {
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

func TestPostgresAccountRuntimeTaskStopProofIsBoundToBothFences(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	runtimeService, err := app.NewAccountAgentRuntimeService(f.store)
	if err != nil {
		t.Fatal(err)
	}
	runtimeNonce := make([]byte, 32)
	runtimeNonce[0] = 91
	runtimeRequest := app.AccountAgentRuntimeStart{OwnerID: f.request.Fence.OwnerID, ConversationID: runtimeTaskConversation(t, f.store, f.request.Fence.TaskID), RuntimeID: "runtime-task-proof",
		LeaseOwner: "portal-runtime-task-proof", SupervisorNonce: runtimeNonce, MaxConcurrentTasks: 2, LeaseDuration: 30 * time.Second}
	runtime, _, err := runtimeService.Start(ctx, runtimeRequest)
	if err != nil {
		t.Fatalf("start account Runtime: %v", err)
	}
	runtime, err = runtimeService.Ready(ctx, runtime.Fence)
	if err != nil {
		t.Fatalf("mark account Runtime ready: %v", err)
	}

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := app.NewAgentStopAuthenticator("supervisor-1", f.key)
	if err != nil {
		t.Fatal(err)
	}
	agentGuardian, err := app.NewAgentGuardianStopAuthenticator("supervisor-1", publicKey)
	if err != nil {
		t.Fatal(err)
	}
	taskGuardian, err := app.NewAccountAgentTaskGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.NewAgentInstanceServiceWithGuardianStops(f.store, supervisor, agentGuardian, taskGuardian)
	if err != nil {
		t.Fatal(err)
	}
	f.request.RuntimeFence = &runtime.Fence
	lease, replayed, err := service.Start(ctx, f.request)
	if err != nil || replayed || lease.RuntimeFence == nil || *lease.RuntimeFence != runtime.Fence {
		t.Fatalf("start Runtime-bound Task Agent: lease=%+v replayed=%t err=%v", lease, replayed, err)
	}
	guardianLeaseExpiry := lease.ExpiresAt
	if _, err := f.store.RenewTaskLease(ctx, lease.Fence.TaskID, lease.Fence.LeaseOwner, lease.Fence.LeaseVersion, app.MaxAgentLease); err != nil {
		t.Fatalf("renew Task lease before Guardian renewal: %v", err)
	}
	// Simulate the Task Agent lease reaching PostgreSQL while Guardian renewal fails.
	lease, err = service.Renew(ctx, lease.Fence, app.MaxAgentLease)
	if err != nil || !lease.ExpiresAt.After(guardianLeaseExpiry) {
		t.Fatalf("renew Task Agent in DB before failed Guardian renewal: lease=%+v err=%v", lease, err)
	}
	taskNonce := sha256.Sum256(f.request.SupervisorNonce)
	attestation := app.AccountAgentTaskStopAttestation{RuntimeFence: runtime.Fence, TaskFence: lease.Fence,
		RuntimeContainerID: strings.Repeat("a", 64), ContainerPID: 7, HostPID: 70, StartTimeTick: 1234,
		CgroupPath: "/sys/fs/cgroup/morde/account-runtime/task-agent", NonceSHA256: hex.EncodeToString(taskNonce[:]),
		ExpiresAt: guardianLeaseExpiry, DestroyedAt: time.Now().UTC(), ProofSHA256: strings.Repeat("b", 64), Destroyed: true}
	payload, err := app.AccountAgentTaskStopAttestationSigningPayload(attestation)
	if err != nil {
		t.Fatal(err)
	}
	attestation.Signature = ed25519.Sign(privateKey, payload)
	if replayed, err := service.ConfirmTaskStopped(ctx, attestation); err != nil || replayed {
		t.Fatalf("confirm account Task process stop: replayed=%t err=%v", replayed, err)
	}
	var state string
	var encoded, digest []byte
	var stoppedAt time.Time
	if err := f.store.pool.QueryRow(ctx, `SELECT state,guardian_stop_attestation,guardian_stop_attestation_digest,stopped_at
FROM task_agent_instances WHERE task_id=$1 AND agent_generation=$2`, lease.Fence.TaskID, lease.Fence.AgentGeneration).
		Scan(&state, &encoded, &digest, &stoppedAt); err != nil {
		t.Fatal(err)
	}
	wantProof, err := json.Marshal(attestation)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := sha256.Sum256(wantProof)
	if state != "stopped" || len(encoded) == 0 || !bytes.Equal(encoded, wantProof) || !bytes.Equal(digest, wantDigest[:]) || stoppedAt.IsZero() {
		t.Fatalf("Task stop proof was not durably stored: state=%s proof=%d digest=%d stopped_at=%v", state, len(encoded), len(digest), stoppedAt)
	}
	if replayed, err := service.ConfirmTaskStopped(ctx, attestation); err != nil || !replayed {
		t.Fatalf("replay account Task stop proof: replayed=%t err=%v", replayed, err)
	}
	pending, err := service.ListPendingAccountAgentTaskStopAttestations(ctx, 1)
	if err != nil || len(pending) != 1 || pending[0].TaskFence != lease.Fence || pending[0].RuntimeFence != runtime.Fence {
		t.Fatalf("list bounded Runtime-bound Task proof outbox: pending=%+v err=%v", pending, err)
	}
	wrongDigest := attestation
	wrongDigest.ProofSHA256 = strings.Repeat("d", 64)
	wrongPayload, err := app.AccountAgentTaskStopAttestationSigningPayload(wrongDigest)
	if err != nil {
		t.Fatal(err)
	}
	wrongDigest.Signature = ed25519.Sign(privateKey, wrongPayload)
	if _, err := service.MarkAccountAgentTaskStopAttestationAcknowledged(ctx, wrongDigest); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("acknowledged a different signed Task proof digest: %v", err)
	}
	if replayed, err := service.MarkAccountAgentTaskStopAttestationAcknowledged(ctx, attestation); err != nil || replayed {
		t.Fatalf("acknowledge Task stop proof: replayed=%t err=%v", replayed, err)
	}
	if replayed, err := service.MarkAccountAgentTaskStopAttestationAcknowledged(ctx, attestation); err != nil || !replayed {
		t.Fatalf("replay Task stop acknowledgement: replayed=%t err=%v", replayed, err)
	}
	pending, err = service.ListPendingAccountAgentTaskStopAttestations(ctx, 1)
	if err != nil || len(pending) != 0 {
		t.Fatalf("acknowledged Task proof remained pending: pending=%+v err=%v", pending, err)
	}

	wrongRuntime := attestation
	wrongRuntime.RuntimeFence.RuntimeGeneration++
	wrongPayload, err = app.AccountAgentTaskStopAttestationSigningPayload(wrongRuntime)
	if err != nil {
		t.Fatal(err)
	}
	wrongRuntime.Signature = ed25519.Sign(privateKey, wrongPayload)
	if _, err := service.ConfirmTaskStopped(ctx, wrongRuntime); domain.CodeOf(err) == "" {
		t.Fatal("accepted a Task stop proof for another Runtime generation")
	}
}

func TestPostgresAccountRuntimeProofStopsUnconfirmedTaskAgents(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	runtimeService, err := app.NewAccountAgentRuntimeService(f.store)
	if err != nil {
		t.Fatal(err)
	}
	runtimeNonce := make([]byte, 32)
	runtimeNonce[0] = 92
	runtime, _, err := runtimeService.Start(ctx, app.AccountAgentRuntimeStart{OwnerID: f.request.Fence.OwnerID, ConversationID: runtimeTaskConversation(t, f.store, f.request.Fence.TaskID),
		RuntimeID: "runtime-parent-stop-proof", LeaseOwner: "portal-parent-stop-proof", SupervisorNonce: runtimeNonce,
		MaxConcurrentTasks: 2, LeaseDuration: 30 * time.Second})
	if err != nil {
		t.Fatalf("start account Runtime: %v", err)
	}
	runtime, err = runtimeService.Ready(ctx, runtime.Fence)
	if err != nil {
		t.Fatalf("ready account Runtime: %v", err)
	}
	f.request.RuntimeFence = &runtime.Fence
	lease, replayed, err := f.service.Start(ctx, f.request)
	if err != nil || replayed || lease.RuntimeFence == nil {
		t.Fatalf("start Runtime-bound Agent: lease=%+v replayed=%t err=%v", lease, replayed, err)
	}
	control, err := f.store.GetExecutionControl(ctx, f.request.Fence.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.StopAllExecution(ctx, executionControlIdempotency(f.request.Fence.OwnerID,
		"execution.stop_all", "runtime-parent-stop-proof"), control.Epoch); err != nil {
		t.Fatalf("request account Runtime stop: %v", err)
	}
	identity, err := runtimeService.StopIdentity(ctx, runtime.Fence.ConversationID)
	if err != nil {
		t.Fatalf("read account Runtime stop identity: %v", err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	proof := signedAccountRuntimeStopProof(t, identity, privateKey)
	runtimeGuardian, err := app.NewAccountAgentRuntimeGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	if replayed, err := runtimeService.ConfirmGuardianStopped(ctx, proof, runtimeGuardian); err != nil || replayed {
		t.Fatalf("confirm account Runtime destruction with active Task Agent: replayed=%t err=%v", replayed, err)
	}
	var state string
	var encoded, digest []byte
	var stoppedAt time.Time
	if err := f.store.pool.QueryRow(ctx, `SELECT state,guardian_stop_attestation,guardian_stop_attestation_digest,stopped_at
FROM task_agent_instances WHERE task_id=$1 AND agent_generation=$2`, lease.Fence.TaskID, lease.Fence.AgentGeneration).
		Scan(&state, &encoded, &digest, &stoppedAt); err != nil {
		t.Fatal(err)
	}
	wantEncoded, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := sha256.Sum256(wantEncoded)
	if state != "stopped" || !bytes.Equal(encoded, wantEncoded) || !bytes.Equal(digest, wantDigest[:]) || stoppedAt.IsZero() {
		t.Fatalf("Runtime proof did not close unconfirmed Task Agent: state=%s proof=%d digest=%d stopped_at=%v", state, len(encoded), len(digest), stoppedAt)
	}
	taskProofs, err := f.store.ListPendingAccountAgentTaskStopAttestations(ctx, 10)
	if err != nil || len(taskProofs) != 0 {
		t.Fatalf("Runtime proof was incorrectly enqueued as a Task proof: pending=%+v err=%v", taskProofs, err)
	}
	runtimeProofs, err := runtimeService.ListPendingAccountAgentRuntimeStopAttestations(ctx, 10, runtimeGuardian)
	if err != nil || len(runtimeProofs) != 1 || runtimeProofs[0].Fence != runtime.Fence {
		t.Fatalf("Runtime proof was not isolated to the Runtime outbox: pending=%+v err=%v", runtimeProofs, err)
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

func TestPostgresAgentHostFenceAllowsTextAndStopsOperationsAfterControlRevocation(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()

	firstHost := createRunningControlHost(t, ctx, f.store, f.principal.UserID, host.ControlAI, 0)
	firstHost = setAgentTestHostRuntime(t, f.store, firstHost, "ri_agent_host_first")
	clientAuthExpiresAt := time.Now().UTC().Add(time.Hour)
	firstRequest := submitHostBoundAgentStart(t, f.store, f.principal.UserID, firstHost.ID,
		task.Authorization{Kind: string(identity.KindClient), TokenID: f.principal.TokenID,
			ClientInstallationID: f.principal.ClientInstallationID, Version: 1, ExpiresAt: &clientAuthExpiresAt},
		"agent-host-first")
	first, replayed, err := f.service.Start(ctx, firstRequest)
	if err != nil || replayed {
		t.Fatalf("start first Host-bound Agent: lease=%+v replayed=%t err=%v", first, replayed, err)
	}
	var storedHostID, storedHostOwner, storedHostActor, storedPermission, storedAuthorizationKind, storedRuntimeInstance, storedGrantFingerprint string
	var storedRuntimeGeneration, storedControlVersion int64
	if err := f.store.pool.QueryRow(ctx, `SELECT host_id,host_owner_id,host_actor_id,host_permission,host_authorization_kind,
host_runtime_generation,host_runtime_instance_id,host_control_version,COALESCE(host_grant_fingerprint,'')
FROM task_agent_instances WHERE task_id=$1 AND agent_generation=$2`, first.Fence.TaskID, first.Fence.AgentGeneration).
		Scan(&storedHostID, &storedHostOwner, &storedHostActor, &storedPermission, &storedAuthorizationKind,
			&storedRuntimeGeneration, &storedRuntimeInstance, &storedControlVersion, &storedGrantFingerprint); err != nil {
		t.Fatalf("read persisted Host Agent fence: %v", err)
	}
	if storedHostID != firstHost.ID || storedHostOwner != f.principal.UserID || storedHostActor != f.principal.UserID ||
		storedPermission != string(host.PermissionDesktopControl) || storedAuthorizationKind != "owner" ||
		storedRuntimeGeneration != firstHost.RuntimeGeneration || storedRuntimeInstance != "ri_agent_host_first" ||
		storedControlVersion != firstHost.ControlVersion || storedGrantFingerprint != "" {
		t.Fatalf("persisted Host Agent fence does not match startup authorization: host=%q owner=%q actor=%q permission=%q kind=%q generation=%d runtime=%q control=%d grantFingerprint=%q",
			storedHostID, storedHostOwner, storedHostActor, storedPermission, storedAuthorizationKind,
			storedRuntimeGeneration, storedRuntimeInstance, storedControlVersion, storedGrantFingerprint)
	}

	beforeRevocation := task.Operation{ID: mustID(t, domain.NewOperationID), TaskID: first.Fence.TaskID,
		Key: "before-host-control-revocation", RequestHash: hashBytes("before host control revocation"), HostID: firstHost.ID,
		RuntimeGeneration: firstHost.RuntimeGeneration, RuntimeInstanceID: "ri_agent_host_first", ControlVersion: firstHost.ControlVersion,
		LeaseVersion: first.Fence.LeaseVersion, RuntimeLeaseVersion: first.Fence.LeaseVersion, Status: task.OperationPrepared}
	if operation, replay, err := f.store.CreateOperation(ctx, beforeRevocation, first.Fence.LeaseOwner); err != nil || replay || operation.Status != task.OperationPrepared {
		t.Fatalf("admit Host operation before control revocation: operation=%+v replay=%t err=%v", operation, replay, err)
	}

	otherOwner := createPreRetirementTestUser(t, f.store, 4, 8_000, 16_000)
	otherHost := createRunningControlHost(t, ctx, f.store, otherOwner.ID, host.ControlAI, 0)
	otherHost = setAgentTestHostRuntime(t, f.store, otherHost, "ri_agent_host_other")
	otherRequest := submitHostBoundAgentStart(t, f.store, otherOwner.ID, otherHost.ID, task.Authorization{}, "agent-host-other")
	otherLease, replayed, err := f.service.Start(ctx, otherRequest)
	if err != nil || replayed {
		t.Fatalf("start another account Host-bound Agent: lease=%+v replayed=%t err=%v", otherLease, replayed, err)
	}

	currentHost, err := f.store.GetHost(ctx, firstHost.ID)
	if err != nil {
		t.Fatal(err)
	}
	completeControlTransitionForTest(t, f.store, firstHost.ID, f.principal.UserID, f.principal.UserID,
		control.ActionTakeOver, currentHost.ControlVersion, "agent-host-first-takeover")

	if _, err := f.service.Renew(ctx, first.Fence, 30*time.Second); err != nil {
		t.Fatalf("Host control handoff stopped unrelated Agent text: %v", err)
	}

	afterRevocation := beforeRevocation
	afterRevocation.ID = mustID(t, domain.NewOperationID)
	afterRevocation.Key = "after-host-control-revocation"
	afterRevocation.RequestHash = hashBytes("after host control revocation")
	if _, _, err := f.store.CreateOperation(ctx, afterRevocation, first.Fence.LeaseOwner); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("Host operation was admitted after control revocation: %v", err)
	}
	if _, err := f.store.GetOperation(ctx, f.principal.UserID, first.Fence.TaskID, afterRevocation.Key); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("Host operation was persisted after control revocation: %v", err)
	}

	// Reopening AI control does not restore the old Agent's authority. The
	// current Host permission is usable again, but its control version has
	// advanced, so the startup snapshot remains stale.
	afterTakeover, err := f.store.GetHost(ctx, firstHost.ID)
	if err != nil || afterTakeover.ControlState != host.ControlUser {
		t.Fatalf("Host after takeover = %#v, err=%v", afterTakeover, err)
	}
	completeControlTransitionForTest(t, f.store, firstHost.ID, f.principal.UserID, f.principal.UserID,
		control.ActionRelease, afterTakeover.ControlVersion, "agent-host-first-release")
	reopenedHost, err := f.store.GetHost(ctx, firstHost.ID)
	if err != nil || reopenedHost.ControlState != host.ControlAI || reopenedHost.ControlVersion == storedControlVersion {
		t.Fatalf("Host after returning control to AI = %#v, err=%v", reopenedHost, err)
	}
	if _, err := f.service.Renew(ctx, first.Fence, 30*time.Second); err != nil {
		t.Fatalf("stale Host control version stopped unrelated Agent text: %v", err)
	}
	staleVersionOperation := afterRevocation
	staleVersionOperation.ID = mustID(t, domain.NewOperationID)
	staleVersionOperation.Key = "after-host-control-returned-to-ai"
	staleVersionOperation.RequestHash = hashBytes("after host control returned to AI")
	if _, _, err := f.store.CreateOperation(ctx, staleVersionOperation, first.Fence.LeaseOwner); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("Host operation accepted the old control version after AI control returned: %v", err)
	}
	if _, err := f.store.GetOperation(ctx, f.principal.UserID, first.Fence.TaskID, staleVersionOperation.Key); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("stale-version Host operation was persisted after AI control returned: %v", err)
	}
	var operationCount int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_operations WHERE task_id=$1`, first.Fence.TaskID).
		Scan(&operationCount); err != nil || operationCount != 1 {
		t.Fatalf("operation count after Host control revocation = %d, err=%v; want only the pre-revocation operation", operationCount, err)
	}

	if _, err := f.service.Renew(ctx, otherLease.Fence, 30*time.Second); err != nil {
		t.Fatalf("Host control revocation for first account affected another account's Agent: %v", err)
	}
}

func TestPostgresAgentHostMemberGrantRevocationAndRegrantFence(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	actorID := f.principal.UserID
	hostOwner := createPreRetirementTestUser(t, f.store, 4, 8_000, 16_000)
	hostValue := createRunningControlHost(t, ctx, f.store, hostOwner.ID, host.ControlAI, 0)
	hostValue = setAgentTestHostRuntime(t, f.store, hostValue, "ri_agent_shared_host_member")
	grant, err := f.store.UpsertHostMember(ctx, host.Member{HostID: hostValue.ID, OwnerID: hostOwner.ID, UserID: actorID,
		Permissions: []host.Permission{host.PermissionDesktopControl}})
	if err != nil {
		t.Fatalf("grant Agent actor desktop:control: %v", err)
	}
	var initialGrantVersion string
	if err := f.store.pool.QueryRow(ctx, `SELECT grant_version::text FROM host_members
WHERE host_id=$1 AND owner_id=$2 AND user_id=$3 AND revoked_at IS NULL`, hostValue.ID, hostOwner.ID, actorID).
		Scan(&initialGrantVersion); err != nil {
		t.Fatalf("read initial Host member grant version: %v", err)
	}

	clientAuthExpiresAt := time.Now().UTC().Add(time.Hour)
	request := submitHostBoundAgentStart(t, f.store, actorID, hostValue.ID,
		task.Authorization{Kind: string(identity.KindClient), TokenID: f.principal.TokenID,
			ClientInstallationID: f.principal.ClientInstallationID, Version: 1, ExpiresAt: &clientAuthExpiresAt},
		"agent-shared-host-member")
	lease, replayed, err := f.service.Start(ctx, request)
	if err != nil || replayed {
		t.Fatalf("start shared Host-bound Agent: lease=%+v replayed=%t err=%v", lease, replayed, err)
	}
	if lease.Fence.OwnerID != actorID || lease.Fence.InitiatorID != actorID {
		t.Fatalf("shared Host task identity was not actor-owned: owner=%q initiator=%q actor=%q", lease.Fence.OwnerID, lease.Fence.InitiatorID, actorID)
	}
	var storedHostOwner, storedHostActor, storedAuthorizationKind, storedGrantFingerprint string
	if err := f.store.pool.QueryRow(ctx, `SELECT host_owner_id,host_actor_id,host_authorization_kind,host_grant_fingerprint
FROM task_agent_instances WHERE task_id=$1 AND agent_generation=$2`, lease.Fence.TaskID, lease.Fence.AgentGeneration).
		Scan(&storedHostOwner, &storedHostActor, &storedAuthorizationKind, &storedGrantFingerprint); err != nil {
		t.Fatalf("read persisted member Agent fence: %v", err)
	}
	if storedHostOwner != hostOwner.ID || storedHostActor != actorID || storedAuthorizationKind != "member" ||
		len(storedGrantFingerprint) != 64 || grant.CreatedAt.IsZero() {
		t.Fatalf("persisted member Agent fence does not capture Host grant: owner=%q actor=%q kind=%q grantFingerprint=%q createdAt=%v",
			storedHostOwner, storedHostActor, storedAuthorizationKind, storedGrantFingerprint, grant.CreatedAt)
	}

	beforeRevoke := task.Operation{ID: mustID(t, domain.NewOperationID), TaskID: lease.Fence.TaskID,
		Key: "shared-host-before-member-revoke", RequestHash: hashBytes("shared Host before member revoke"), HostID: hostValue.ID,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_agent_shared_host_member", ControlVersion: hostValue.ControlVersion,
		LeaseVersion: lease.Fence.LeaseVersion, RuntimeLeaseVersion: lease.Fence.LeaseVersion, Status: task.OperationPrepared}
	if operation, replay, err := f.store.CreateOperation(ctx, beforeRevoke, lease.Fence.LeaseOwner); err != nil || replay || operation.Status != task.OperationPrepared {
		t.Fatalf("admit operation under active Host member grant: operation=%+v replay=%t err=%v", operation, replay, err)
	}
	if _, err := f.store.UpdateOperation(ctx, app.OperationUpdate{TaskID: beforeRevoke.TaskID, OperationKey: beforeRevoke.Key,
		LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_agent_shared_host_member",
		ControlVersion: hostValue.ControlVersion, RuntimeLeaseVersion: lease.Fence.LeaseVersion, Status: task.OperationDispatched}); err != nil {
		t.Fatalf("dispatch operation before member grant revocation: %v", err)
	}
	if _, err := f.store.UpdateOperation(ctx, app.OperationUpdate{TaskID: beforeRevoke.TaskID, OperationKey: beforeRevoke.Key,
		LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_agent_shared_host_member",
		ControlVersion: hostValue.ControlVersion, RuntimeLeaseVersion: lease.Fence.LeaseVersion,
		Status: task.OperationUnknown, ExternalID: "uncertain-before-revoke"}); err != nil {
		t.Fatalf("record uncertain operation before member grant revocation: %v", err)
	}
	preparedBeforeRevoke := task.Operation{ID: mustID(t, domain.NewOperationID), TaskID: lease.Fence.TaskID,
		Key: "shared-host-prepared-before-member-revoke", RequestHash: hashBytes("shared Host prepared before member revoke"), HostID: hostValue.ID,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_agent_shared_host_member", ControlVersion: hostValue.ControlVersion,
		LeaseVersion: lease.Fence.LeaseVersion, RuntimeLeaseVersion: lease.Fence.LeaseVersion, Status: task.OperationPrepared}
	if operation, replay, err := f.store.CreateOperation(ctx, preparedBeforeRevoke, lease.Fence.LeaseOwner); err != nil || replay || operation.Status != task.OperationPrepared {
		t.Fatalf("admit second operation under active Host member grant: operation=%+v replay=%t err=%v", operation, replay, err)
	}
	pendingSubmission := newTestTaskSubmission(t, actorID, hostValue.ID, "shared-host-queued-before-revoke", "queued before Host grant revocation")
	pendingTask, err := f.store.SubmitTask(ctx, pendingSubmission)
	if err != nil {
		t.Fatalf("submit second Task under active Host member grant: %v", err)
	}

	if err := f.store.RevokeHostMember(ctx, hostValue.ID, hostOwner.ID, actorID); err != nil {
		t.Fatalf("revoke Agent actor Host grant: %v", err)
	}
	if _, err := f.service.Renew(ctx, lease.Fence, 30*time.Second); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("Host-bound Agent renewed after member grant revocation: %v", err)
	}
	assertSharedHostOperationRejected(t, f.store, beforeRevoke, lease.Fence.OwnerID, lease.Fence.LeaseOwner, "shared-host-after-member-revoke")
	assertSharedHostOperationRearmRejected(t, f.store, beforeRevoke, lease.Fence, hostValue, "after member revoke")
	assertSharedHostOperationDispatchRejected(t, f.store, preparedBeforeRevoke, lease.Fence, hostValue, "after member revoke")
	if claimed, found, err := f.store.ClaimNextTask(ctx, "agent-shared-host-after-revoke", 30*time.Second); err != nil || found || claimed.ID == pendingTask.Task.ID {
		t.Fatalf("Host member revocation did not fence queued Task claim: found=%t claimed=%+v err=%v", found, claimed, err)
	}

	// UpsertHostMember starts a new grant by replacing created_at. The old
	// Agent must remain fenced even though the same permission is granted again.
	time.Sleep(2 * time.Millisecond)
	regrant, err := f.store.UpsertHostMember(ctx, host.Member{HostID: hostValue.ID, OwnerID: hostOwner.ID, UserID: actorID,
		Permissions: []host.Permission{host.PermissionDesktopControl}})
	if err != nil {
		t.Fatalf("regrant Agent actor desktop:control: %v", err)
	}
	if !regrant.CreatedAt.After(grant.CreatedAt) {
		t.Fatalf("regrant did not create a new grant version: first=%v regrant=%v", grant.CreatedAt, regrant.CreatedAt)
	}
	var regrantVersion string
	if err := f.store.pool.QueryRow(ctx, `SELECT grant_version::text FROM host_members
WHERE host_id=$1 AND owner_id=$2 AND user_id=$3 AND revoked_at IS NULL`, hostValue.ID, hostOwner.ID, actorID).
		Scan(&regrantVersion); err != nil {
		t.Fatalf("read replacement Host member grant version: %v", err)
	}
	if regrantVersion == initialGrantVersion {
		t.Fatalf("Host member regrant reused grant version %q", regrantVersion)
	}
	if _, err := f.service.Renew(ctx, lease.Fence, 30*time.Second); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("old Host-bound Agent renewed after member grant was replaced: %v", err)
	}
	assertSharedHostOperationRejected(t, f.store, beforeRevoke, lease.Fence.OwnerID, lease.Fence.LeaseOwner, "shared-host-after-member-regrant")
	assertSharedHostOperationRearmRejected(t, f.store, beforeRevoke, lease.Fence, hostValue, "after member regrant")
	assertSharedHostOperationDispatchRejected(t, f.store, preparedBeforeRevoke, lease.Fence, hostValue, "after member regrant")
	var operationCount int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_operations WHERE task_id=$1`, lease.Fence.TaskID).
		Scan(&operationCount); err != nil || operationCount != 2 {
		t.Fatalf("operation count after member grant revoke/regrant = %d, err=%v; want only the two pre-revocation operations", operationCount, err)
	}
}

func TestPostgresAgentHostOfflineAllowsTextButRejectsOperations(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	computer := createRunningControlHost(t, ctx, f.store, f.principal.UserID, host.ControlAI, 0)
	computer = setAgentTestHostRuntime(t, f.store, computer, "ri_offline_task_host")
	expires := time.Now().UTC().Add(time.Hour)
	request := submitHostBoundAgentStart(t, f.store, f.principal.UserID, computer.ID,
		task.Authorization{Kind: string(identity.KindClient), TokenID: f.principal.TokenID,
			ClientInstallationID: f.principal.ClientInstallationID, Version: 1, ExpiresAt: &expires}, "agent-host-offline")
	lease, _, err := f.service.Start(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE hosts SET state='stopped' WHERE id=$1`, computer.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Renew(ctx, lease.Fence, 30*time.Second); err != nil {
		t.Fatalf("offline Host interrupted the Agent's text execution: %v", err)
	}
	operation := task.Operation{ID: mustID(t, domain.NewOperationID), TaskID: lease.Fence.TaskID,
		Key: "offline-host-operation", RequestHash: hashBytes("offline host operation"), HostID: computer.ID,
		RuntimeGeneration: computer.RuntimeGeneration, RuntimeInstanceID: "ri_offline_task_host", ControlVersion: computer.ControlVersion,
		LeaseVersion: lease.Fence.LeaseVersion, RuntimeLeaseVersion: lease.Fence.LeaseVersion, Status: task.OperationPrepared}
	if _, _, err := f.store.CreateOperation(ctx, operation, lease.Fence.LeaseOwner); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("offline Host admitted a computer operation: %v", err)
	}
	replacement := request
	replacement.Fence.AgentInstanceID = "agent-host-offline-replacement"
	replacement.SupervisorNonce = bytes.Repeat([]byte{8}, 32)
	if _, _, err := f.service.Start(ctx, replacement); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("unconfirmed Agent was replaced while Host offline: %v", err)
	}
	receipt := signAgentLifecycleReceipt(t, f.key, lease.Fence, request.SupervisorNonce)
	if _, err := f.service.ConfirmStopped(ctx, receipt); err != nil {
		t.Fatalf("confirm old Agent stop: %v", err)
	}
	restarted, _, err := f.service.Start(ctx, replacement)
	if err != nil || restarted.Fence.AgentGeneration != lease.Fence.AgentGeneration+1 {
		t.Fatalf("offline Host prevented verified Agent recovery: lease=%+v err=%v", restarted, err)
	}
	if _, err := f.service.Renew(ctx, restarted.Fence, 30*time.Second); err != nil {
		t.Fatalf("recovered Agent could not continue text while Host offline: %v", err)
	}
}

func TestPostgresAgentRecoveryRejectsRegrantedHostPermission(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, f.store, 2, 2000, 4096)
	computer := createRunningControlHost(t, ctx, f.store, owner.ID, host.ControlAI, 0)
	computer = setAgentTestHostRuntime(t, f.store, computer, "ri_regrant_recovery")
	grant := host.Member{HostID: computer.ID, OwnerID: owner.ID, UserID: f.principal.UserID,
		Permissions: []host.Permission{host.PermissionDesktopControl}}
	if _, err := f.store.UpsertHostMember(ctx, grant); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Hour)
	request := submitHostBoundAgentStart(t, f.store, f.principal.UserID, computer.ID,
		task.Authorization{Kind: string(identity.KindClient), TokenID: f.principal.TokenID,
			ClientInstallationID: f.principal.ClientInstallationID, Version: 1, ExpiresAt: &expires}, "agent-regrant-recovery")
	lease, _, err := f.service.Start(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ConfirmStopped(ctx, signAgentLifecycleReceipt(t, f.key, lease.Fence, request.SupervisorNonce)); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RevokeHostMember(ctx, computer.ID, owner.ID, f.principal.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.UpsertHostMember(ctx, grant); err != nil {
		t.Fatal(err)
	}
	replacement := request
	replacement.Fence.AgentInstanceID = "agent-regrant-replacement"
	replacement.SupervisorNonce = bytes.Repeat([]byte{7}, 32)
	if _, _, err := f.service.Start(ctx, replacement); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("regranted Host permission revived an old Agent: %v", err)
	}
}

func assertSharedHostOperationDispatchRejected(t *testing.T, store *Store, operation task.Operation, fence app.AgentInstanceFence,
	hostValue host.Host, phase string) {
	t.Helper()
	_, err := store.UpdateOperation(context.Background(), app.OperationUpdate{
		TaskID: operation.TaskID, OperationKey: operation.Key, LeaseOwner: fence.LeaseOwner, LeaseVersion: fence.LeaseVersion,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: hostValue.RuntimeInstanceID,
		ControlVersion: hostValue.ControlVersion, RuntimeLeaseVersion: fence.LeaseVersion, Status: task.OperationDispatched,
	})
	if domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("Host operation dispatched %s after member grant changed: %v", phase, err)
	}
	stored, err := store.GetOperation(context.Background(), fence.OwnerID, operation.TaskID, operation.Key)
	if err != nil || stored.Status != task.OperationPrepared {
		t.Fatalf("Host operation state after rejected dispatch %s = %q, err=%v; want prepared", phase, stored.Status, err)
	}
}

func assertSharedHostOperationRejected(t *testing.T, store *Store, original task.Operation, ownerID, leaseOwner, key string) {
	t.Helper()
	value := original
	value.ID = mustID(t, domain.NewOperationID)
	value.Key = key
	value.RequestHash = hashBytes(key)
	if _, _, err := store.CreateOperation(context.Background(), value, leaseOwner); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("Host operation %q was admitted after member grant changed: %v", key, err)
	}
	if _, err := store.GetOperation(context.Background(), ownerID, original.TaskID, key); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("Host operation %q was persisted after member grant changed: %v", key, err)
	}
}

func assertSharedHostOperationRearmRejected(t *testing.T, store *Store, operation task.Operation, fence app.AgentInstanceFence,
	hostValue host.Host, phase string) {
	t.Helper()
	_, err := store.UpdateOperation(context.Background(), app.OperationUpdate{
		TaskID: operation.TaskID, OperationKey: operation.Key, LeaseOwner: fence.LeaseOwner, LeaseVersion: fence.LeaseVersion,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: hostValue.RuntimeInstanceID,
		ControlVersion: hostValue.ControlVersion, RuntimeLeaseVersion: fence.LeaseVersion,
		Status: task.OperationPrepared, RearmAfterNotFound: true,
	})
	if domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("Host operation rearmed %s after member grant changed: %v", phase, err)
	}
	stored, err := store.GetOperation(context.Background(), fence.OwnerID, operation.TaskID, operation.Key)
	if err != nil || stored.Status != task.OperationUnknown {
		t.Fatalf("Host operation state after rejected rearm %s = %q, err=%v; want unknown", phase, stored.Status, err)
	}
}

func TestPostgresAgentHostlessLeaseRenewalDoesNotRequireHostFence(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	lease, replayed, err := f.service.Start(context.Background(), f.request)
	if err != nil || replayed {
		t.Fatalf("start Hostless Agent: lease=%+v replayed=%t err=%v", lease, replayed, err)
	}
	if _, err := f.service.Renew(context.Background(), lease.Fence, 30*time.Second); err != nil {
		t.Fatalf("Hostless Agent renewal unexpectedly required a Host fence: %v", err)
	}
}

func setAgentTestHostRuntime(t *testing.T, store *Store, value host.Host, runtimeInstanceID string) host.Host {
	t.Helper()
	ctx := context.Background()
	current, err := store.GetHost(ctx, value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetRuntimeInstanceID(ctx, current.ID, current.RuntimeGeneration, current.ControlVersion, "", runtimeInstanceID); err != nil {
		t.Fatalf("set Host runtime identity: %v", err)
	}
	current, err = store.GetHost(ctx, value.ID)
	if err != nil {
		t.Fatal(err)
	}
	return current
}

func submitHostBoundAgentStart(t *testing.T, store *Store, ownerID, hostID string, authorization task.Authorization,
	idempotencyKey string) app.AgentInstanceStart {
	t.Helper()
	ctx := context.Background()
	submission := newTestTaskSubmission(t, ownerID, hostID, idempotencyKey, "run Host-bound Agent")
	if authorization.Kind != "" {
		submission.Task.Authorization = authorization
	}
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit Host-bound Agent task: %v", err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "agent-host-"+idempotencyKey, time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		var status, hostID, executionLocation string
		var executionEpoch int64
		_ = store.pool.QueryRow(ctx, `SELECT status,COALESCE(host_id,''),execution_location,execution_epoch
FROM tasks WHERE id=$1`, created.Task.ID).Scan(&status, &hostID, &executionLocation, &executionEpoch)
		t.Fatalf("claim Host-bound Agent task: claimed=%t task=%+v status=%q host=%q execution_location=%q execution_epoch=%d err=%v",
			found, claimed, status, hostID, executionLocation, executionEpoch, err)
	}
	nonce := sha256.Sum256([]byte("agent-host-nonce:" + idempotencyKey))
	return app.AgentInstanceStart{
		Fence: app.AgentInstanceFence{TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
			AgentInstanceID: "agent-" + idempotencyKey, AgentSessionID: "session-" + idempotencyKey,
			SupervisorID: "supervisor-1", LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
			AuthorizationVersion: claimed.Authorization.Version, EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1"},
		SupervisorNonce: nonce[:], LeaseDuration: 30 * time.Second,
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

func TestPostgresAgentGuardianStopAttestationRecoveryAndGenerationFence(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	first, replayed, err := f.service.Start(ctx, f.request)
	if err != nil || replayed || first.Fence.AgentGeneration != 1 {
		t.Fatalf("start first Agent: lease=%+v replayed=%t err=%v", first, replayed, err)
	}
	supervisor, err := app.NewAgentStopAuthenticator("supervisor-1", f.key)
	if err != nil {
		t.Fatal(err)
	}
	guardianPrivate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x72}, ed25519.SeedSize))
	guardian, err := app.NewAgentGuardianStopAuthenticator("supervisor-1", guardianPrivate.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.NewAgentInstanceServiceWithGuardianStop(f.store, supervisor, guardian)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := service.GuardianStopIdentity(ctx, first.Fence.TaskID, first.Fence.OwnerID, first.Fence.AgentInstanceID, first.Fence.AgentGeneration)
	nonceHash := sha256.Sum256(f.request.SupervisorNonce)
	if err != nil || identity.Fence != first.Fence || identity.NonceSHA256 != hex.EncodeToString(nonceHash[:]) {
		t.Fatalf("load exact active Guardian recovery identity: identity=%+v err=%v", identity, err)
	}
	if _, err := service.GuardianStopIdentity(ctx, first.Fence.TaskID, "u9999999", first.Fence.AgentInstanceID, first.Fence.AgentGeneration); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("mismatched owner returned an active recovery identity: %v", err)
	}
	if _, err := service.GuardianStopIdentity(ctx, first.Fence.TaskID, first.Fence.OwnerID, first.Fence.AgentInstanceID, first.Fence.AgentGeneration+1); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("stale generation returned an active recovery identity: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	attestation := app.AgentGuardianStopAttestation{
		Fence: identity.Fence, ContainerID: string(bytes.Repeat([]byte("a"), 64)), ContainerName: "morde-dsh-" + string(bytes.Repeat([]byte("b"), 64)),
		NonceSHA256: identity.NonceSHA256, ExpiresAt: now.Add(-time.Second), DestroyedAt: now,
		ProofSHA256: string(bytes.Repeat([]byte("c"), 64)), Destroyed: true,
	}
	payload, err := app.AgentGuardianStopAttestationSigningPayload(attestation)
	if err != nil {
		t.Fatal(err)
	}
	attestation.Signature = ed25519.Sign(guardianPrivate, payload)
	if replayed, err := service.ConfirmGuardianStopped(ctx, attestation); err != nil || replayed {
		t.Fatalf("record Guardian stop attestation: replayed=%t err=%v", replayed, err)
	}
	var state string
	var stored, storedDigest []byte
	if err := f.store.pool.QueryRow(ctx, `SELECT state,guardian_stop_attestation,guardian_stop_attestation_digest
FROM task_agent_instances WHERE task_id=$1 AND agent_generation=1`, first.Fence.TaskID).Scan(&state, &stored, &storedDigest); err != nil {
		t.Fatal(err)
	}
	attestationDigest := sha256.Sum256(stored)
	if state != "stopped" || len(stored) == 0 || !bytes.Equal(storedDigest, attestationDigest[:]) || bytes.Contains(stored, f.request.SupervisorNonce) {
		t.Fatalf("Guardian proof storage is invalid: state=%q proof=%q digest=%x", state, stored, storedDigest)
	}
	pendingAccountTaskProofs, err := f.store.ListPendingAccountAgentTaskStopAttestations(ctx, 10)
	if err != nil || len(pendingAccountTaskProofs) != 0 {
		t.Fatalf("legacy non-Runtime Guardian proof entered the account Task acknowledgement outbox: pending=%+v err=%v", pendingAccountTaskProofs, err)
	}
	if replayed, err := service.ConfirmGuardianStopped(ctx, attestation); err != nil || !replayed {
		t.Fatalf("exact Guardian stop replay: replayed=%t err=%v", replayed, err)
	}
	changed := attestation
	changed.DestroyedAt = changed.DestroyedAt.Add(time.Second)
	payload, err = app.AgentGuardianStopAttestationSigningPayload(changed)
	if err != nil {
		t.Fatal(err)
	}
	changed.Signature = ed25519.Sign(guardianPrivate, payload)
	if _, err := service.ConfirmGuardianStopped(ctx, changed); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("different proof for the same Agent generation was accepted: %v", err)
	}

	next := f.request
	next.Fence.AgentInstanceID = "agent-2"
	next.SupervisorNonce = bytes.Repeat([]byte{0x24}, 32)
	second, replayed, err := f.service.Start(ctx, next)
	if err != nil || replayed || second.Fence.AgentGeneration != 2 {
		t.Fatalf("Guardian proof did not authorize a fenced next generation: lease=%+v replayed=%t err=%v", second, replayed, err)
	}
	if _, err := service.GuardianStopIdentity(ctx, first.Fence.TaskID, first.Fence.OwnerID, first.Fence.AgentInstanceID, first.Fence.AgentGeneration); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("stale stopped generation remained a current recovery identity: %v", err)
	}
	current, err := service.GuardianStopIdentity(ctx, second.Fence.TaskID, second.Fence.OwnerID, second.Fence.AgentInstanceID, second.Fence.AgentGeneration)
	if err != nil || current.Fence != second.Fence {
		t.Fatalf("current generation recovery lookup: identity=%+v err=%v", current, err)
	}
}
