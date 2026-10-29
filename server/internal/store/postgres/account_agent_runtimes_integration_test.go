package postgres

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

func TestPostgresAccountAgentRuntimeLeaseAndTaskSequence(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	runtimeService, err := app.NewAccountAgentRuntimeService(f.store)
	if err != nil {
		t.Fatal(err)
	}
	request := app.AccountAgentRuntimeStart{OwnerID: f.request.Fence.OwnerID, RuntimeID: "account-runtime-1",
		LeaseOwner: "portal-runtime-1", SupervisorNonce: append([]byte(nil), f.request.SupervisorNonce...),
		MaxConcurrentTasks: 2, LeaseDuration: 30 * time.Second}
	runtime, replayed, err := runtimeService.Start(ctx, request)
	if err != nil || replayed || runtime.State != app.AccountAgentRuntimeStarting || runtime.RuntimeSequence != 0 || runtime.Fence.RuntimeGeneration != 1 {
		t.Fatalf("start account runtime: runtime=%+v replayed=%t err=%v", runtime, replayed, err)
	}
	instanceID, err := f.store.ServiceInstanceID(ctx)
	if err != nil || runtime.Fence.ServiceInstanceID != instanceID {
		t.Fatalf("runtime service identity = %q, want %q, err=%v", runtime.Fence.ServiceInstanceID, instanceID, err)
	}
	if replay, wasReplay, err := runtimeService.Start(ctx, request); err != nil || !wasReplay || replay.Fence != runtime.Fence {
		t.Fatalf("idempotent runtime start: runtime=%+v replayed=%t err=%v", replay, wasReplay, err)
	}
	changed := request
	changed.RuntimeID = "account-runtime-2"
	if _, _, err := runtimeService.Start(ctx, changed); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("replaced a live account runtime without stop proof: %v", err)
	}
	runtime, err = runtimeService.Ready(ctx, runtime.Fence)
	if err != nil || runtime.State != app.AccountAgentRuntimeRunning {
		t.Fatalf("mark account runtime ready: runtime=%+v err=%v", runtime, err)
	}
	if _, err := runtimeService.Ready(ctx, runtime.Fence); err != nil {
		t.Fatalf("idempotent runtime ready: %v", err)
	}

	f.request.RuntimeFence = &runtime.Fence
	first, replayed, err := f.service.Start(ctx, f.request)
	if err != nil || replayed || first.RuntimeSequence != 1 || first.RuntimeFence == nil || *first.RuntimeFence != runtime.Fence {
		t.Fatalf("start first Task Agent: lease=%+v replayed=%t err=%v", first, replayed, err)
	}
	if again, wasReplay, err := f.service.Start(ctx, f.request); err != nil || !wasReplay || again.RuntimeSequence != first.RuntimeSequence {
		t.Fatalf("replay Task Agent start: lease=%+v replayed=%t err=%v", again, wasReplay, err)
	}

	receipt := signAgentLifecycleReceipt(t, f.key, first.Fence, f.request.SupervisorNonce)
	if _, err := f.service.ConfirmStopped(ctx, receipt); err != nil {
		t.Fatalf("confirm first Task Agent stopped: %v", err)
	}
	next := f.request
	next.Fence.AgentInstanceID = "agent-next"
	next.SupervisorNonce = make([]byte, 32)
	next.SupervisorNonce[0] = 2
	second, replayed, err := f.service.Start(ctx, next)
	if err != nil || replayed || second.Fence.AgentGeneration != 2 || second.RuntimeSequence != 2 {
		t.Fatalf("start recovered Task Agent: lease=%+v replayed=%t err=%v", second, replayed, err)
	}

	original, err := f.store.GetTask(ctx, f.request.Fence.TaskID, f.request.Fence.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	submission := newTestTaskSubmission(t, f.request.Fence.OwnerID, "", "account-runtime-parallel-task", "run parallel task")
	submission.Task.Authorization = original.Task.Authorization
	created, err := f.store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := f.store.ClaimNextTaskWithOwnerLimit(ctx, "runtime-parallel-worker", 25*time.Second, 2)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim parallel Task: task=%s found=%t err=%v", claimed.ID, found, err)
	}
	secondTaskStart := next
	secondTaskStart.Fence.TaskID = claimed.ID
	secondTaskStart.Fence.AgentInstanceID = "agent-parallel"
	secondTaskStart.Fence.AgentSessionID = "session-parallel"
	secondTaskStart.Fence.LeaseOwner = claimed.Lease.Owner
	secondTaskStart.Fence.LeaseVersion = claimed.Lease.Version
	secondTaskStart.Fence.AuthorizationVersion = claimed.Authorization.Version
	secondTaskStart.SupervisorNonce = make([]byte, 32)
	secondTaskStart.SupervisorNonce[0] = 3

	stopSecond := signAgentLifecycleReceipt(t, f.key, second.Fence, next.SupervisorNonce)
	if _, err := f.service.ConfirmStopped(ctx, stopSecond); err != nil {
		t.Fatalf("confirm recovered Task Agent stopped before concurrent restart: %v", err)
	}
	restartFirstTask := next
	restartFirstTask.Fence.AgentInstanceID = "agent-third"
	restartFirstTask.SupervisorNonce = make([]byte, 32)
	restartFirstTask.SupervisorNonce[0] = 4
	type startResult struct {
		lease    app.AgentInstanceLease
		replayed bool
		err      error
	}
	starts := make(chan startResult, 2)
	for _, request := range []app.AgentInstanceStart{restartFirstTask, secondTaskStart} {
		go func(request app.AgentInstanceStart) {
			lease, replayed, err := f.service.Start(ctx, request)
			starts <- startResult{lease: lease, replayed: replayed, err: err}
		}(request)
	}
	parallelOne, parallelTwo := <-starts, <-starts
	if parallelOne.err != nil || parallelTwo.err != nil || parallelOne.replayed || parallelTwo.replayed ||
		parallelOne.lease.RuntimeSequence == parallelTwo.lease.RuntimeSequence ||
		min(parallelOne.lease.RuntimeSequence, parallelTwo.lease.RuntimeSequence) != 3 ||
		max(parallelOne.lease.RuntimeSequence, parallelTwo.lease.RuntimeSequence) != 4 {
		t.Fatalf("parallel account Runtime starts: first=%+v second=%+v", parallelOne, parallelTwo)
	}
	var renewed app.AgentInstanceLease
	var taskRuntimeSequence int64
	for _, value := range []app.AgentInstanceLease{parallelOne.lease, parallelTwo.lease} {
		if value.Fence.TaskID == f.request.Fence.TaskID {
			taskRuntimeSequence = value.RuntimeSequence
			renewed, err = f.service.Renew(ctx, value.Fence, 15*time.Second)
			break
		}
	}
	if err != nil || renewed.RuntimeSequence != taskRuntimeSequence || renewed.RuntimeFence == nil || renewed.RuntimeFence.RuntimeID != runtime.Fence.RuntimeID {
		t.Fatalf("renew Task Agent under account Runtime: lease=%+v err=%v", renewed, err)
	}

	if _, err := f.store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='disabled',epoch=epoch+1 WHERE owner_id=$1`, request.OwnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeService.Renew(ctx, runtime.Fence, 15*time.Second); domain.CodeOf(err) == "" {
		t.Fatal("account runtime lease renewed after execution authorization changed")
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled',epoch=epoch+1 WHERE owner_id=$1`, request.OwnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeService.Renew(ctx, runtime.Fence, 15*time.Second); err == nil {
		t.Fatal("account runtime lease renewed after the owner gate was re-enabled at a newer epoch")
	}
	if _, err := f.service.Renew(ctx, renewed.Fence, 15*time.Second); err == nil {
		t.Fatal("Task Agent lease renewed under a stale account Runtime execution epoch")
	}
}

func TestPostgresAccountRuntimeCrashRecoveryRequiresExpiredLease(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	runtimeService, err := app.NewAccountAgentRuntimeService(store)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	nonce[0] = 61
	runtime, replayed, err := runtimeService.Start(ctx, app.AccountAgentRuntimeStart{OwnerID: owner.ID,
		RuntimeID: "runtime-crash-recovery", LeaseOwner: "portal-crash-recovery", SupervisorNonce: nonce,
		MaxConcurrentTasks: 2, LeaseDuration: 30 * time.Second})
	if err != nil || replayed {
		t.Fatalf("start account Runtime: runtime=%+v replayed=%t err=%v", runtime, replayed, err)
	}
	runtime, err = runtimeService.Ready(ctx, runtime.Fence)
	if err != nil {
		t.Fatalf("ready account Runtime: %v", err)
	}
	identity, err := runtimeService.StopIdentity(ctx, owner.ID)
	if err != nil {
		t.Fatalf("read Runtime stop identity: %v", err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	proof := signedAccountRuntimeStopProof(t, identity, privateKey)
	authenticator, err := app.NewAccountAgentRuntimeGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeService.ConfirmGuardianStopped(ctx, proof, authenticator); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("accepted Guardian stop proof for an unexpired Runtime under enabled gate: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE account_agent_runtimes SET lease_expires_at=clock_timestamp()-interval '1 second'
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND runtime_id=$3 AND runtime_generation=$4`,
		runtime.Fence.ServiceInstanceID, owner.ID, runtime.Fence.RuntimeID, runtime.Fence.RuntimeGeneration); err != nil {
		t.Fatal(err)
	}
	if replayed, err := runtimeService.ConfirmGuardianStopped(ctx, proof, authenticator); err != nil || replayed {
		t.Fatalf("confirm expired Runtime destruction with enabled gate: replayed=%t err=%v", replayed, err)
	}
	pending, err := runtimeService.ListPendingAccountAgentRuntimeStopAttestations(ctx, 1, authenticator)
	if err != nil || len(pending) != 1 || pending[0].Fence != runtime.Fence {
		t.Fatalf("list pending Runtime stop proof: pending=%+v err=%v", pending, err)
	}
	if _, err := runtimeService.ListPendingAccountAgentRuntimeStopAttestations(ctx, 0, authenticator); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("accepted unbounded Runtime stop outbox query: %v", err)
	}
	wrongDigestProof := proof
	wrongDigestProof.ProofSHA256 = strings.Repeat("c", 64)
	wrongPayload, err := app.AccountAgentRuntimeStopAttestationSigningPayload(wrongDigestProof)
	if err != nil {
		t.Fatal(err)
	}
	wrongDigestProof.Signature = ed25519.Sign(privateKey, wrongPayload)
	if _, err := runtimeService.MarkAccountAgentRuntimeStopAttestationAcknowledged(ctx, wrongDigestProof, authenticator); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("acknowledged a different signed Runtime proof digest: %v", err)
	}
	if replayed, err := runtimeService.MarkAccountAgentRuntimeStopAttestationAcknowledged(ctx, proof, authenticator); err != nil || replayed {
		t.Fatalf("acknowledge pending Runtime stop proof: replayed=%t err=%v", replayed, err)
	}
	if replayed, err := runtimeService.ConfirmGuardianStopped(ctx, proof, authenticator); err != nil || !replayed {
		t.Fatalf("replay already-acknowledged Runtime stop proof: replayed=%t err=%v", replayed, err)
	}
	if replayed, err := runtimeService.MarkAccountAgentRuntimeStopAttestationAcknowledged(ctx, proof, authenticator); err != nil || !replayed {
		t.Fatalf("replay Runtime stop acknowledgement: replayed=%t err=%v", replayed, err)
	}
	pending, err = runtimeService.ListPendingAccountAgentRuntimeStopAttestations(ctx, 1, authenticator)
	if err != nil || len(pending) != 0 {
		t.Fatalf("acknowledged Runtime proof remained pending: pending=%+v err=%v", pending, err)
	}
	control, err := store.GetExecutionControl(ctx, owner.ID)
	if err != nil || control.State != "enabled" || control.Runtime.State != "stopped" {
		t.Fatalf("crash recovery changed the enabled owner gate: control=%#v err=%v", control, err)
	}
	if replayed, err := runtimeService.ConfirmGuardianStopped(ctx, proof, authenticator); err != nil || !replayed {
		t.Fatalf("replay crash-recovery Runtime proof: replayed=%t err=%v", replayed, err)
	}
}

func TestPostgresAbortedAccountRuntimeStartRequiresExpiredLeaseAndNoWork(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	service, err := app.NewAccountAgentRuntimeService(store)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	nonce[0] = 81
	request := app.AccountAgentRuntimeStart{OwnerID: owner.ID, RuntimeID: "runtime-aborted-start",
		LeaseOwner: "portal-aborted-start", SupervisorNonce: nonce, MaxConcurrentTasks: 2, LeaseDuration: 30 * time.Second}
	runtime, replayed, err := service.Start(ctx, request)
	if err != nil || replayed {
		t.Fatalf("start Runtime: runtime=%+v replayed=%t err=%v", runtime, replayed, err)
	}
	identity, err := service.StopIdentity(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := app.NewAccountAgentRuntimeGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	proof := signedAbortedAccountRuntimeStartProof(t, identity, privateKey)
	if _, err := service.ConfirmGuardianStopped(ctx, proof, verifier); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("accepted aborted start proof before lease expired: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE account_agent_runtimes SET lease_expires_at=clock_timestamp()-interval '1 second'
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND runtime_id=$3`, runtime.Fence.ServiceInstanceID, owner.ID, runtime.Fence.RuntimeID); err != nil {
		t.Fatal(err)
	}
	if replayed, err := service.ConfirmGuardianStopped(ctx, proof, verifier); err != nil || replayed {
		t.Fatalf("confirm aborted start: replayed=%t err=%v", replayed, err)
	}
	loaded, found, err := service.Get(ctx, owner.ID)
	if err != nil || !found || loaded.State != app.AccountAgentRuntimeStopped {
		t.Fatalf("aborted start did not stop Runtime: runtime=%+v found=%t err=%v", loaded, found, err)
	}
	pending, err := service.ListPendingAccountAgentRuntimeStopAttestations(ctx, 1, verifier)
	if err != nil || len(pending) != 0 {
		t.Fatalf("no-Guardian-lease proof incorrectly needs acknowledgement: pending=%+v err=%v", pending, err)
	}
	laterProof := signedAbortedAccountRuntimeStartProof(t, identity, privateKey)
	if replayed, err := service.ConfirmGuardianStopped(ctx, laterProof, verifier); err != nil || !replayed {
		t.Fatalf("second signed aborted-start observation was not idempotent: replayed=%t err=%v", replayed, err)
	}
	request.RuntimeID = "runtime-after-abort"
	request.SupervisorNonce = make([]byte, 32)
	request.SupervisorNonce[0] = 82
	replacement, replayed, err := service.Start(ctx, request)
	if err != nil || replayed || replacement.Fence.RuntimeGeneration != runtime.Fence.RuntimeGeneration+1 {
		t.Fatalf("start replacement Runtime: runtime=%+v replayed=%t err=%v", replacement, replayed, err)
	}
	if _, err := service.Ready(ctx, replacement.Fence); err != nil {
		t.Fatalf("mark replacement Runtime running: %v", err)
	}
	runningIdentity, err := service.StopIdentity(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	runningProof := signedAbortedAccountRuntimeStartProof(t, runningIdentity, privateKey)
	if _, err := pool.Exec(ctx, `UPDATE account_agent_runtimes SET lease_expires_at=clock_timestamp()-interval '1 second'
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND runtime_id=$3`, replacement.Fence.ServiceInstanceID, owner.ID, replacement.Fence.RuntimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConfirmGuardianStopped(ctx, runningProof, verifier); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("accepted aborted-start proof for a running Runtime: %v", err)
	}
	stillRunning, found, err := service.Get(ctx, owner.ID)
	if err != nil || !found || stillRunning.State != app.AccountAgentRuntimeRunning {
		t.Fatalf("rejected aborted-start proof changed running Runtime: runtime=%+v found=%t err=%v", stillRunning, found, err)
	}
}

func signedAbortedAccountRuntimeStartProof(t *testing.T, identity app.AccountAgentRuntimeStopIdentity,
	privateKey ed25519.PrivateKey) app.AccountAgentRuntimeStopAttestation {
	t.Helper()
	proof := app.AccountAgentRuntimeStopAttestation{Fence: identity.Fence,
		ContainerName: accountRuntimeContainerNameForTest(identity.Fence, identity.NonceSHA256),
		NonceSHA256:   identity.NonceSHA256, DestroyedAt: time.Now().UTC(), ProofSHA256: strings.Repeat("e", 64),
		Destroyed: true, StartAborted: true}
	payload, err := app.AccountAgentRuntimeStopAttestationSigningPayload(proof)
	if err != nil {
		t.Fatal(err)
	}
	proof.Signature = ed25519.Sign(privateKey, payload)
	return proof
}

func TestPostgresListsBoundedAccountRuntimeStopCandidates(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	runtimeService, err := app.NewAccountAgentRuntimeService(store)
	if err != nil {
		t.Fatal(err)
	}
	start := func(ownerID, id string, nonceByte byte) app.AccountAgentRuntime {
		t.Helper()
		nonce := make([]byte, 32)
		nonce[0] = nonceByte
		runtime, replayed, err := runtimeService.Start(ctx, app.AccountAgentRuntimeStart{OwnerID: ownerID,
			RuntimeID: id, LeaseOwner: "portal-" + id, SupervisorNonce: nonce,
			MaxConcurrentTasks: 1, LeaseDuration: 30 * time.Second})
		if err != nil || replayed {
			t.Fatalf("start account Runtime %s: runtime=%+v replayed=%t err=%v", id, runtime, replayed, err)
		}
		return runtime
	}
	owners := make([]string, 6)
	for index := range owners {
		owners[index] = createPreRetirementTestUser(t, store, 8, 20_000, 40_000).ID
	}
	stopping := start(owners[0], "runtime-candidate-stop", 71)
	startingExpired := start(owners[1], "runtime-candidate-starting", 72)
	runningExpired := start(owners[2], "runtime-candidate-running", 73)
	if _, err := runtimeService.Ready(ctx, runningExpired.Fence); err != nil {
		t.Fatal(err)
	}
	failedExpired := start(owners[3], "runtime-candidate-failed", 74)
	start(owners[4], "runtime-candidate-live", 75)
	stopped := start(owners[5], "runtime-candidate-stopped", 76)
	if _, err := store.pool.Exec(ctx, `UPDATE account_agent_runtimes SET state='stopping' WHERE owner_id=$1 AND runtime_id=$2`, owners[0], stopping.Fence.RuntimeID); err != nil {
		t.Fatal(err)
	}
	for _, runtime := range []app.AccountAgentRuntime{startingExpired, runningExpired, failedExpired, stopped} {
		if _, err := store.pool.Exec(ctx, `UPDATE account_agent_runtimes SET lease_expires_at=clock_timestamp()-interval '1 second'
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND runtime_id=$3`, runtime.Fence.ServiceInstanceID, runtime.Fence.OwnerID, runtime.Fence.RuntimeID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.pool.Exec(ctx, `UPDATE account_agent_runtimes SET state='failed' WHERE owner_id=$1 AND runtime_id=$2`, owners[3], failedExpired.Fence.RuntimeID); err != nil {
		t.Fatal(err)
	}
	identity, err := runtimeService.StopIdentity(ctx, owners[5])
	if err != nil {
		t.Fatalf("read stopped-case identity before proof: %v", err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	proof := signedAccountRuntimeStopProof(t, identity, privateKey)
	authenticator, err := app.NewAccountAgentRuntimeGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeService.ConfirmGuardianStopped(ctx, proof, authenticator); err != nil {
		t.Fatalf("confirm stopped-case Runtime proof: %v", err)
	}
	candidates, err := runtimeService.ListAccountAgentRuntimeStopCandidates(ctx, app.MaxAccountAgentRuntimeStopCandidates)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{owners[0]: true, owners[1]: true, owners[2]: true, owners[3]: true}
	for _, candidate := range candidates {
		if !want[candidate.Fence.OwnerID] || candidate.NonceSHA256 == "" || candidate.ExpiresAt.IsZero() {
			t.Fatalf("unexpected or incomplete Runtime stop candidate: %+v", candidate)
		}
		delete(want, candidate.Fence.OwnerID)
	}
	if len(candidates) != 4 || len(want) != 0 {
		t.Fatalf("Runtime stop candidates = %+v, missing owners=%v", candidates, want)
	}
	limited, err := runtimeService.ListAccountAgentRuntimeStopCandidates(ctx, 2)
	if err != nil || len(limited) != 2 {
		t.Fatalf("bounded Runtime stop candidate query: candidates=%+v err=%v", limited, err)
	}
}
