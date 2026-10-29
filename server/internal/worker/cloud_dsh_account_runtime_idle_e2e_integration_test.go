package worker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
)

func TestAccountAgentRuntimeIdleRenewalPersistsBeyondOriginalExpiry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	control, err := app.NewAccountAgentRuntimeService(store)
	if err != nil {
		t.Fatal(err)
	}
	supervisor := &accountRuntimeSupervisorStub{}
	manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{
		Control: control, Supervisor: supervisor, Guardian: &accountRuntimeGuardianStub{},
		StopVerifier: accountRuntimeStopVerifierStub{}, TaskStopper: &accountRuntimeTaskStopperStub{},
		LeaseOwner: "worker-idle-runtime", MaxTasks: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := manager.Acquire(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.process.Input.Close(); _ = session.process.Output.Close() }()
	if _, err := pool.Exec(ctx, `UPDATE account_agent_runtimes SET lease_expires_at=clock_timestamp()+interval '5 seconds'
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND runtime_id=$3`,
		session.runtime.Fence.ServiceInstanceID, owner.ID, session.runtime.Fence.RuntimeID); err != nil {
		t.Fatal(err)
	}
	session.renewMu.Lock()
	session.leaseExpiresAt = time.Now().Add(5 * time.Second)
	session.renewMu.Unlock()
	if err := manager.renewIdleSessions(ctx); err != nil {
		t.Fatal(err)
	}
	persisted, found, err := control.Get(ctx, owner.ID)
	if err != nil || !found || persisted.State != app.AccountAgentRuntimeRunning ||
		persisted.LeaseExpiresAt.Before(time.Now().Add(20*time.Second)) || persisted.Fence != session.runtime.Fence {
		t.Fatalf("idle Runtime was not kept alive in PostgreSQL: runtime=%+v found=%t err=%v", persisted, found, err)
	}
	candidates, err := control.ListAccountAgentRuntimeStopCandidates(ctx, 10)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("renewed idle Runtime was selected for cleanup: candidates=%+v err=%v", candidates, err)
	}
	supervisor.mu.Lock()
	renewCalls := supervisor.renewCalls
	supervisor.mu.Unlock()
	if renewCalls != 1 || session.closed() {
		t.Fatalf("Guardian renewal count=%d, session closed=%t", renewCalls, session.closed())
	}
}

func TestAccountRuntimeRecoversAbortedStartAcrossManagerRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	control, err := app.NewAccountAgentRuntimeService(store)
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
	guardian := &accountRuntimeGuardianStub{}
	newManager := func(supervisor CloudDSHAccountRuntimeSupervisor) *CloudDSHAccountRuntimeManager {
		t.Helper()
		manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{
			Control: control, Supervisor: supervisor, Guardian: guardian, StopVerifier: verifier,
			TaskStopper: &accountRuntimeTaskStopperStub{}, LeaseOwner: "worker-restarted-runtime", MaxTasks: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		return manager
	}
	first := newManager(&accountRuntimeSupervisorStub{incomplete: true})
	if _, err := first.Acquire(ctx, owner.ID); err == nil {
		t.Fatal("incomplete first startup unexpectedly succeeded")
	}
	old, found, err := control.Get(ctx, owner.ID)
	if err != nil || !found || old.State != app.AccountAgentRuntimeStarting {
		t.Fatalf("failed startup state=%+v found=%t err=%v", old, found, err)
	}
	restarted := newManager(&signedRuntimeRecoverySupervisor{accountRuntimeSupervisorStub: &accountRuntimeSupervisorStub{}, key: privateKey, aborted: true})
	if _, err := restarted.Acquire(ctx, owner.ID); err == nil {
		t.Fatal("replacement Runtime started before the old lease expired")
	}
	if _, err := pool.Exec(ctx, `UPDATE account_agent_runtimes SET lease_expires_at=clock_timestamp()-interval '1 second'
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND runtime_id=$3`,
		old.Fence.ServiceInstanceID, owner.ID, old.Fence.RuntimeID); err != nil {
		t.Fatal(err)
	}
	session, err := restarted.Acquire(ctx, owner.ID)
	if err != nil {
		t.Fatalf("restart Manager failed to replace aborted Runtime: %v", err)
	}
	defer func() { _ = session.process.Input.Close(); _ = session.process.Output.Close() }()
	current, found, err := control.Get(ctx, owner.ID)
	if err != nil || !found || current.State != app.AccountAgentRuntimeRunning ||
		current.Fence.RuntimeGeneration != old.Fence.RuntimeGeneration+1 || current.Fence.RuntimeID == old.Fence.RuntimeID {
		t.Fatalf("replacement Runtime state=%+v found=%t err=%v", current, found, err)
	}
	if _, err := control.Ready(ctx, old.Fence); err == nil {
		t.Fatal("old Runtime fence became ready after replacement")
	}
	pending, err := control.ListPendingAccountAgentRuntimeStopAttestations(ctx, 1, verifier)
	if err != nil || len(pending) != 0 || len(guardian.runtimeProofAcks) != 0 {
		t.Fatalf("aborted start required nonexistent Guardian ACK: pending=%+v ACKs=%d err=%v", pending, len(guardian.runtimeProofAcks), err)
	}
}

func TestAccountRuntimeReplacesStoppedOldProcessAcrossManagerRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	control, err := app.NewAccountAgentRuntimeService(store)
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
	guardian := &accountRuntimeGuardianStub{}
	first, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{
		Control: control, Supervisor: &accountRuntimeSupervisorStub{}, Guardian: guardian, StopVerifier: verifier,
		TaskStopper: &accountRuntimeTaskStopperStub{}, LeaseOwner: "first-portal-runtime", MaxTasks: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	oldSession, err := first.Acquire(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldFence := oldSession.runtime.Fence
	_ = oldSession.process.Input.Close()
	_ = oldSession.process.Output.Close()
	for deadline := time.Now().Add(time.Second); !oldSession.closed() && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if !oldSession.closed() {
		t.Fatal("old Runtime session did not close before recovery")
	}
	restarted, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{
		Control: control, Supervisor: &signedRuntimeRecoverySupervisor{accountRuntimeSupervisorStub: &accountRuntimeSupervisorStub{}, key: privateKey},
		Guardian: guardian, StopVerifier: verifier, TaskStopper: &accountRuntimeTaskStopperStub{},
		LeaseOwner: "second-portal-runtime", MaxTasks: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Acquire(ctx, owner.ID); err == nil {
		t.Fatal("replacement Runtime started before the old lease expired")
	}
	if _, err := pool.Exec(ctx, `UPDATE account_agent_runtimes SET lease_expires_at=clock_timestamp()-interval '1 second'
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND runtime_id=$3`,
		oldFence.ServiceInstanceID, owner.ID, oldFence.RuntimeID); err != nil {
		t.Fatal(err)
	}
	session, err := restarted.Acquire(ctx, owner.ID)
	if err != nil {
		t.Fatalf("restart Manager failed to replace old Runtime: %v", err)
	}
	defer func() { _ = session.process.Input.Close(); _ = session.process.Output.Close() }()
	current, found, err := control.Get(ctx, owner.ID)
	if err != nil || !found || current.State != app.AccountAgentRuntimeRunning ||
		current.Fence.RuntimeGeneration != oldFence.RuntimeGeneration+1 || current.Fence.RuntimeID == oldFence.RuntimeID {
		t.Fatalf("replacement Runtime state=%+v found=%t err=%v", current, found, err)
	}
	if _, err := control.Renew(ctx, oldFence, 15*time.Second); err == nil {
		t.Fatal("old Runtime fence renewed after replacement")
	}
	pending, err := control.ListPendingAccountAgentRuntimeStopAttestations(ctx, 1, verifier)
	if err != nil || len(pending) != 0 || len(guardian.runtimeProofAcks) != 1 {
		t.Fatalf("old Runtime stop proof ACK state: pending=%+v ACKs=%d err=%v", pending, len(guardian.runtimeProofAcks), err)
	}
}

type signedRuntimeRecoverySupervisor struct {
	*accountRuntimeSupervisorStub
	key     ed25519.PrivateKey
	aborted bool
}

func (s *signedRuntimeRecoverySupervisor) RecoverAccountRuntimeStopped(_ context.Context, fence app.AccountAgentRuntimeFence,
	nonceSHA256 string) (app.AccountAgentRuntimeStopAttestation, error) {
	nonce, err := hex.DecodeString(nonceSHA256)
	if err != nil {
		return app.AccountAgentRuntimeStopAttestation{}, err
	}
	encodedFence, err := json.Marshal(fence)
	if err != nil {
		return app.AccountAgentRuntimeStopAttestation{}, err
	}
	identity := sha256.Sum256(append(append(encodedFence, 0), nonce...))
	proof := app.AccountAgentRuntimeStopAttestation{Fence: fence,
		ContainerName: "morde-account-runtime-" + hex.EncodeToString(identity[:]), NonceSHA256: nonceSHA256,
		DestroyedAt: time.Now().UTC(), ProofSHA256: strings.Repeat("e", 64), Destroyed: true, StartAborted: s.aborted}
	if !s.aborted {
		proof.ContainerID = strings.Repeat("a", 64)
		proof.ExpiresAt = time.Now().Add(-time.Second)
	}
	payload, err := app.AccountAgentRuntimeStopAttestationSigningPayload(proof)
	if err != nil {
		return app.AccountAgentRuntimeStopAttestation{}, err
	}
	proof.Signature = ed25519.Sign(s.key, payload)
	return proof, nil
}
