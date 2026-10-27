package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"testing"
	"time"

	"morde/server/internal/domain"
)

type agentInstanceStoreStub struct {
	startCalls int
	stopCalls  int
}

func (s *agentInstanceStoreStub) StartAgentInstance(_ context.Context, request AgentInstanceStart) (AgentInstanceLease, bool, error) {
	s.startCalls++
	return AgentInstanceLease{Fence: request.Fence}, false, nil
}

func (*agentInstanceStoreStub) RenewAgentInstance(_ context.Context, fence AgentInstanceFence, _ time.Duration) (AgentInstanceLease, error) {
	return AgentInstanceLease{Fence: fence}, nil
}

func (s *agentInstanceStoreStub) ConfirmAgentStopped(_ context.Context, value VerifiedAgentStop) (bool, error) {
	receipt, encoded := value.Value()
	if !receipt.TreeStopped || len(encoded) == 0 {
		return false, domain.InvalidArgument("receipt", "invalid receipt")
	}
	s.stopCalls++
	return false, nil
}

func agentInstanceTestFence() AgentInstanceFence {
	return AgentInstanceFence{TaskID: "q1234567", OwnerID: "u1234567", AgentInstanceID: "agent-1",
		AgentSessionID: "session-1", SupervisorID: "supervisor-1", LeaseOwner: "worker-1", LeaseVersion: 1,
		AuthorizationVersion: 1, EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1"}
}

func signedAgentTestReceipt(t *testing.T, key []byte, fence AgentInstanceFence, nonce []byte) AgentStopReceipt {
	t.Helper()
	receipt := AgentStopReceipt{Fence: fence, SupervisorNonce: nonce, TreeStopped: true}
	payload, err := AgentStopReceiptSigningPayload(receipt)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	receipt.Signature = mac.Sum(nil)
	return receipt
}

func TestAgentInstanceServiceVerifiesSupervisorReceipt(t *testing.T) {
	key := make([]byte, 32)
	key[0] = 3
	verifier, err := NewAgentStopAuthenticator("supervisor-1", key)
	if err != nil {
		t.Fatal(err)
	}
	store := &agentInstanceStoreStub{}
	service, err := NewAgentInstanceService(store, verifier)
	if err != nil {
		t.Fatal(err)
	}
	fence := agentInstanceTestFence()
	fence.AgentGeneration = 1
	nonce := make([]byte, 32)
	nonce[0] = 7
	receipt := signedAgentTestReceipt(t, key, fence, nonce)
	altered := receipt
	altered.Fence.AgentGeneration++
	if _, err := service.ConfirmStopped(context.Background(), altered); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("tampered generation was accepted: %v", err)
	}
	altered = receipt
	altered.TreeStopped = false
	if _, err := service.ConfirmStopped(context.Background(), altered); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("unconfirmed process tree was accepted: %v", err)
	}
	if store.stopCalls != 0 {
		t.Fatal("invalid receipt reached persistence")
	}
	if _, err := service.ConfirmStopped(context.Background(), receipt); err != nil || store.stopCalls != 1 {
		t.Fatalf("valid receipt rejected: calls=%d err=%v", store.stopCalls, err)
	}
}

func TestAgentInstanceLeaseHasHardThirtySecondLimit(t *testing.T) {
	key := make([]byte, 32)
	key[0] = 1
	verifier, err := NewAgentStopAuthenticator("supervisor-1", key)
	if err != nil {
		t.Fatal(err)
	}
	store := &agentInstanceStoreStub{}
	service, err := NewAgentInstanceService(store, verifier)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	nonce[0] = 1
	request := AgentInstanceStart{Fence: agentInstanceTestFence(), SupervisorNonce: nonce, LeaseDuration: 31 * time.Second}
	if _, _, err := service.Start(context.Background(), request); domain.CodeOf(err) != domain.CodeInvalidArgument || store.startCalls != 0 {
		t.Fatalf("overlong lease accepted: %v", err)
	}
	request.LeaseDuration = 30 * time.Second
	if _, _, err := service.Start(context.Background(), request); err != nil || store.startCalls != 1 {
		t.Fatalf("bounded lease rejected: %v", err)
	}
}
