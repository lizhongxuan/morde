package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
)

type agentInstanceStoreStub struct {
	startCalls            int
	stopCalls             int
	guardianStopCalls     int
	guardianAttestation   AgentGuardianStopAttestation
	guardianIdentity      AgentGuardianStopIdentity
	guardianIdentityCalls int
	pendingTaskStops      []AccountAgentTaskStopAttestation
	pendingTaskStopLimit  int
	acknowledgedTaskStop  AccountAgentTaskStopAttestation
	taskStopAckCalls      int
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

func (s *agentInstanceStoreStub) ConfirmAgentGuardianStopped(_ context.Context, value VerifiedAgentGuardianStop) (bool, error) {
	attestation, encoded := value.Value()
	if !attestation.Destroyed || len(encoded) == 0 {
		return false, domain.InvalidArgument("attestation", "invalid Guardian attestation")
	}
	s.guardianStopCalls++
	s.guardianAttestation = attestation
	return false, nil
}

func (s *agentInstanceStoreStub) ActiveAgentGuardianStopIdentity(_ context.Context, taskID, ownerID, agentInstanceID string, generation int64) (AgentGuardianStopIdentity, error) {
	s.guardianIdentityCalls++
	if s.guardianIdentity.Fence.TaskID != taskID || s.guardianIdentity.Fence.OwnerID != ownerID ||
		s.guardianIdentity.Fence.AgentInstanceID != agentInstanceID || s.guardianIdentity.Fence.AgentGeneration != generation {
		return AgentGuardianStopIdentity{}, domain.NewError(domain.CodeInvalidState, "active Agent identity unavailable")
	}
	return s.guardianIdentity, nil
}

func (s *agentInstanceStoreStub) ListPendingAccountAgentTaskStopAttestations(_ context.Context, limit int) ([]AccountAgentTaskStopAttestation, error) {
	s.pendingTaskStopLimit = limit
	if len(s.pendingTaskStops) > limit {
		return append([]AccountAgentTaskStopAttestation(nil), s.pendingTaskStops[:limit]...), nil
	}
	return append([]AccountAgentTaskStopAttestation(nil), s.pendingTaskStops...), nil
}

func (s *agentInstanceStoreStub) MarkAccountAgentTaskStopAttestationAcknowledged(_ context.Context,
	proof VerifiedAccountAgentTaskStop) (bool, error) {
	s.acknowledgedTaskStop, _ = proof.Value()
	s.taskStopAckCalls++
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

func signedAgentGuardianTestAttestation(t *testing.T, key ed25519.PrivateKey, fence AgentInstanceFence, nonce []byte) AgentGuardianStopAttestation {
	t.Helper()
	nonceHash := sha256.Sum256(nonce)
	attestation := AgentGuardianStopAttestation{
		Fence: fence, ContainerID: strings.Repeat("a", 64), ContainerName: "morde-dsh-" + strings.Repeat("b", 64),
		NonceSHA256: hex.EncodeToString(nonceHash[:]), ExpiresAt: time.Now().UTC().Add(-time.Second),
		DestroyedAt: time.Now().UTC(), ProofSHA256: strings.Repeat("c", 64), Destroyed: true,
	}
	payload, err := AgentGuardianStopAttestationSigningPayload(attestation)
	if err != nil {
		t.Fatal(err)
	}
	attestation.Signature = ed25519.Sign(key, payload)
	return attestation
}

func signedAccountAgentTaskStopTestAttestation(t *testing.T, key ed25519.PrivateKey) AccountAgentTaskStopAttestation {
	t.Helper()
	now := time.Now().UTC()
	attestation := AccountAgentTaskStopAttestation{
		RuntimeFence: AccountAgentRuntimeFence{ServiceInstanceID: "12345678-1234-1234-1234-123456789abc", OwnerID: "u1234567",
			RuntimeID: "runtime-1", RuntimeGeneration: 1, LeaseOwner: "runtime-worker", LeaseVersion: 1, ExecutionEpoch: 1},
		TaskFence: AgentInstanceFence{TaskID: "q1234567", OwnerID: "u1234567", AgentInstanceID: "agent-1", AgentGeneration: 1,
			AgentSessionID: "session-1", SupervisorID: "supervisor-1", LeaseOwner: "worker-1", LeaseVersion: 1,
			AuthorizationVersion: 1, EngineVersion: "v1", ProfileVersion: "p1"},
		RuntimeContainerID: strings.Repeat("a", 64), ContainerPID: 2, HostPID: 3, StartTimeTick: 4,
		CgroupPath: "/sys/fs/cgroup/morde-task", NonceSHA256: strings.Repeat("b", 64),
		ExpiresAt: now.Add(time.Minute), DestroyedAt: now, ProofSHA256: strings.Repeat("c", 64), Destroyed: true,
	}
	payload, err := AccountAgentTaskStopAttestationSigningPayload(attestation)
	if err != nil {
		t.Fatal(err)
	}
	attestation.Signature = ed25519.Sign(key, payload)
	return attestation
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

func TestAgentInstanceServiceVerifiesDistinctGuardianAttestationAndLoadsExactRecoveryIdentity(t *testing.T) {
	supervisorKey := make([]byte, 32)
	supervisorKey[0] = 3
	supervisor, err := NewAgentStopAuthenticator("supervisor-1", supervisorKey)
	if err != nil {
		t.Fatal(err)
	}
	guardianKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x72}, ed25519.SeedSize))
	guardian, err := NewAgentGuardianStopAuthenticator("supervisor-1", guardianKey.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	store := &agentInstanceStoreStub{}
	service, err := NewAgentInstanceServiceWithGuardianStop(store, supervisor, guardian)
	if err != nil {
		t.Fatal(err)
	}
	fence := agentInstanceTestFence()
	fence.AgentGeneration = 2
	nonce := bytes.Repeat([]byte{0x19}, 32)
	attestation := signedAgentGuardianTestAttestation(t, guardianKey, fence, nonce)
	altered := attestation
	altered.Fence.AgentGeneration++
	if _, err := service.ConfirmGuardianStopped(context.Background(), altered); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("tampered Agent generation was accepted: %v", err)
	}
	altered = attestation
	altered.Destroyed = false
	if _, err := service.ConfirmGuardianStopped(context.Background(), altered); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("unconfirmed destruction was accepted: %v", err)
	}
	if store.guardianStopCalls != 0 {
		t.Fatal("invalid Guardian attestation reached persistence")
	}
	if _, err := service.ConfirmGuardianStopped(context.Background(), attestation); err != nil || store.guardianStopCalls != 1 {
		t.Fatalf("valid Guardian attestation rejected: calls=%d err=%v", store.guardianStopCalls, err)
	}
	if store.guardianAttestation.NonceSHA256 != attestation.NonceSHA256 || len(store.guardianAttestation.Signature) != ed25519.SignatureSize {
		t.Fatalf("stored Guardian attestation = %#v", store.guardianAttestation)
	}

	store.guardianIdentity = AgentGuardianStopIdentity{Fence: fence, NonceSHA256: attestation.NonceSHA256}
	identity, err := service.GuardianStopIdentity(context.Background(), fence.TaskID, fence.OwnerID, fence.AgentInstanceID, fence.AgentGeneration)
	if err != nil || identity != store.guardianIdentity || store.guardianIdentityCalls != 1 {
		t.Fatalf("GuardianStopIdentity() = %#v, calls=%d err=%v", identity, store.guardianIdentityCalls, err)
	}
	if _, err := service.GuardianStopIdentity(context.Background(), fence.TaskID, fence.OwnerID, fence.AgentInstanceID, fence.AgentGeneration+1); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("stale Agent generation returned a recovery identity: %v", err)
	}
}

func TestAgentInstanceServiceVerifiesPendingTaskStopProofsBeforeAcknowledging(t *testing.T) {
	supervisorKey := make([]byte, 32)
	supervisorKey[0] = 3
	supervisor, err := NewAgentStopAuthenticator("supervisor-1", supervisorKey)
	if err != nil {
		t.Fatal(err)
	}
	guardianKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x73}, ed25519.SeedSize))
	guardian, err := NewAccountAgentTaskGuardianStopAuthenticator(guardianKey.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	attestation := signedAccountAgentTaskStopTestAttestation(t, guardianKey)
	store := &agentInstanceStoreStub{pendingTaskStops: []AccountAgentTaskStopAttestation{attestation}}
	service, err := NewAgentInstanceService(store, supervisor)
	if err != nil {
		t.Fatal(err)
	}
	service.accountTaskGuardianAuthenticator = guardian

	if _, err := service.ListPendingAccountAgentTaskStopAttestations(context.Background(), 0); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("accepted an invalid pending proof limit: %v", err)
	}
	if _, err := service.ListPendingAccountAgentTaskStopAttestations(context.Background(), MaxPendingAccountAgentTaskStopAttestations+1); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("accepted an oversized pending proof limit: %v", err)
	}
	pending, err := service.ListPendingAccountAgentTaskStopAttestations(context.Background(), 3)
	if err != nil || len(pending) != 1 || store.pendingTaskStopLimit != 3 || pending[0].TaskFence != attestation.TaskFence {
		t.Fatalf("load pending Guardian proofs: pending=%+v limit=%d err=%v", pending, store.pendingTaskStopLimit, err)
	}
	if replayed, err := service.MarkAccountAgentTaskStopAttestationAcknowledged(context.Background(), attestation); err != nil || replayed ||
		store.taskStopAckCalls != 1 || store.acknowledgedTaskStop.ProofSHA256 != attestation.ProofSHA256 {
		t.Fatalf("mark verified Guardian proof acknowledged: replayed=%t calls=%d proof=%+v err=%v", replayed, store.taskStopAckCalls, store.acknowledgedTaskStop, err)
	}
	attestation.Signature[0] ^= 0xff
	if _, err := service.MarkAccountAgentTaskStopAttestationAcknowledged(context.Background(), attestation); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("acknowledged a tampered Guardian proof: %v", err)
	}
	store.pendingTaskStops[0].Signature[0] ^= 0xff
	if _, err := service.ListPendingAccountAgentTaskStopAttestations(context.Background(), 3); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("returned a corrupted stored Guardian proof: %v", err)
	}
}

func TestAgentGuardianStopAuthenticatorMustBeSeparate(t *testing.T) {
	supervisorKey := bytes.Repeat([]byte{0x31}, 32)
	supervisor, err := NewAgentStopAuthenticator("supervisor-1", supervisorKey)
	if err != nil {
		t.Fatal(err)
	}
	public := ed25519.PublicKey(bytes.Repeat([]byte{0x31}, ed25519.PublicKeySize))
	guardian, err := NewAgentGuardianStopAuthenticator("supervisor-1", public)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewAgentInstanceServiceWithGuardianStop(&agentInstanceStoreStub{}, supervisor, guardian); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("shared HMAC/public key bytes accepted: %v", err)
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
